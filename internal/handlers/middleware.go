package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// ctxKey is the private context key type for the authenticated user.
type ctxKey struct{}

// CurrentUser returns the authenticated user placed in ctx by RequireUser.
func CurrentUser(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

// RequireUser authenticates the request from either the sd_session cookie or
// a Bearer API key (architecture doc §4). The two are ALTERNATIVES — either
// one alone authenticates a request, and they are never both required. The
// cookie is tried first; if absent, an `Authorization: Bearer sd_live_…`
// header is resolved to the admin/agent who minted the key, carrying that
// key's scopes. On success the user is stored in the request context;
// otherwise a 401 problem is written, with no detail (the caller learns only
// that authentication failed).
//
// Scope gate: an API-key request to a mutating (non-safe) operation is
// refused 403 unless the key carries the `write` scope. Cookie sessions are
// never scope-limited. The gate lives here so it applies uniformly across
// every /api/v1 operation without per-handler wiring.
func (h *Handlers) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unauthorized := func() {
			problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired credentials")
		}

		// 1. Session cookie (browsers).
		if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
			sess, err := auth.VerifySession(h.secret, c.Value)
			if err != nil {
				unauthorized()
				return
			}
			u, err := h.q.GetUserByID(r.Context(), sess.UserID)
			if err != nil {
				if isNoRows(err) {
					unauthorized()
					return
				}
				serverError(w, r, "load session user", err)
				return
			}
			// token_version mismatch means the session was revoked (password
			// change, deactivation, logout-everywhere); inactive users are
			// locked out immediately regardless of cookie validity.
			if !u.Active || sess.TokenVersion != u.TokenVersion {
				unauthorized()
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
			return
		}

		// 2. Bearer API key (machines). Resolution, the scope gate, and the
		// best-effort usage stamp live in apikey_auth.go.
		if token, ok := bearerToken(r.Header.Get("Authorization")); ok {
			principal, err := h.authenticateAPIKey(r.Context(), token)
			if err != nil {
				if errors.Is(err, errInvalidAPIKey) {
					unauthorized()
					return
				}
				serverError(w, r, "authenticate api key", err)
				return
			}
			if !enforceAPIKeyScope(w, r, principal.scopes) {
				return // 403 already written
			}
			h.touchAPIKeyAsync(principal.keyID)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, principal.user)))
			return
		}

		unauthorized()
	})
}

// RequireRole allows only authenticated users whose role is one of roles.
// It must run after RequireUser (no user in context reads as a 401, not a
// 500).
func RequireRole(roles ...api.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := CurrentUser(r.Context())
			if !ok {
				problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
				return
			}
			for _, role := range roles {
				if api.Role(u.Role) == role {
					next.ServeHTTP(w, r)
					return
				}
			}
			problem.Write(w, r, http.StatusForbidden, "Forbidden", "insufficient role")
		})
	}
}

// ticketCoreSuffixes are the route patterns of the M2 ticket-core surface,
// which requires an agent or admin session (openapi.yaml access model).
// Customer access arrives with the M4 portal.
var ticketCoreSuffixes = []string{
	"/tickets",
	"/tickets/{id}",
	"/tickets/{id}/tags",
	"/tickets/{id}/articles",
	"/articles/{id}/attachments",
	"/articles/{id}/retry-send",
	"/attachments/{id}",
	"/tags",
	"/tags/{id}",
	"/dashboard/counters",
	"/events",
}

// isTicketCorePattern reports whether the resolved chi route pattern belongs
// to the agent/admin ticket-core surface.
func isTicketCorePattern(pattern string) bool {
	return hasAnySuffix(pattern, ticketCoreSuffixes)
}

// agentReadSuffixes are the user/team routes whose GETs the workspace needs
// below admin: the assignee picker lists agents/admins, the team select
// lists teams, and the context sidebar enriches the requester. Mutations on
// these resources remain admin-only (authPolicy default).
var agentReadSuffixes = []string{
	"/users",
	"/users/{id}",
	"/teams",
	"/teams/{id}",
}

// isAgentReadPattern reports whether the resolved chi route pattern is a
// user/team read agents may perform.
func isAgentReadPattern(pattern string) bool {
	return hasAnySuffix(pattern, agentReadSuffixes)
}

func hasAnySuffix(pattern string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(pattern, suffix) {
			return true
		}
	}
	return false
}

// setupSuffixes are the first-run installer routes. They are exempt from the
// setup-incomplete 503 gate (they ARE the setup surface) and unauthenticated
// (createSetupAdmin carries the installer token; the later steps use the
// admin session established there).
var setupSuffixes = []string{
	"/setup/status",
	"/setup/admin",
	"/setup/instance",
	"/setup/complete",
}

func isSetupPattern(pattern string) bool { return hasAnySuffix(pattern, setupSuffixes) }

// portalSuffixes are the customer self-service routes (customer role only —
// the hard isolation boundary).
var portalSuffixes = []string{
	"/portal/tickets",
	"/portal/tickets/{id}",
	"/portal/tickets/{id}/reply",
}

func isPortalPattern(pattern string) bool { return hasAnySuffix(pattern, portalSuffixes) }

// cannedReplySuffixes are the canned-reply routes (agent or admin).
var cannedReplySuffixes = []string{
	"/canned-replies",
	"/canned-replies/{id}",
}

func isCannedReplyPattern(pattern string) bool { return hasAnySuffix(pattern, cannedReplySuffixes) }

// isSetupComplete reports whether first-run setup has finished. Once true it
// is cached in-process forever (setup never un-completes), so the gate below
// costs a settings read only during first-run. A read error fails open (the
// operation's own DB access will surface the fault) rather than 503-ing a
// healthy, already-set-up instance on a transient blip.
func (h *Handlers) isSetupComplete(ctx context.Context) bool {
	if h.setupCompleted.Load() {
		return true
	}
	done, err := h.settings.SetupCompleted(ctx)
	if err != nil {
		return true
	}
	if done {
		h.setupCompleted.Store(true)
	}
	return done
}

// authPolicy maps each operation to its access rule, deny-by-default:
//
//   - /auth/login, /auth/logout: public (spec: security [])
//   - /auth/me:                  any authenticated user
//   - DELETE /tags/{id}:         admin only (the one stricter M2 rule)
//   - ticket core (tickets, articles incl. retry-send, attachments, tags,
//     dashboard, events):        agent or admin
//   - GET on users/teams:        agent or admin (workspace pickers)
//   - everything else:           admin only (users/teams management, the
//     whole M3 /mailboxes + /settings surface incl. the Google OAuth
//     callback)
//
// It runs inside the generated route wrapper, so the chi route pattern is
// already resolved; matching on the pattern suffix keeps the policy
// independent of the mount point (/api/v1).
func (h *Handlers) authPolicy(next http.Handler) http.Handler {
	requireUser := h.RequireUser(next)
	requireAdmin := h.RequireUser(RequireRole(api.RoleAdmin)(next))
	requireAgent := h.RequireUser(RequireRole(api.RoleAgent, api.RoleAdmin)(next))
	requireCustomer := h.RequireUser(RequireRole(api.RoleCustomer)(next))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := chi.RouteContext(r.Context()).RoutePattern()

		// Setup-incomplete gate (M4): before first-run setup finishes, the
		// only usable /api/v1 surface is the installer itself; everything
		// else answers 503 {setup required}. The setup routes and the public
		// web form (which needs no admin) are exempt; health lives outside
		// /api/v1 and is unaffected.
		if !isSetupPattern(pattern) && !strings.HasSuffix(pattern, "/public/tickets") &&
			!h.isSetupComplete(r.Context()) {
			problem.Write(w, r, http.StatusServiceUnavailable, "Setup Required",
				"first-run setup is not complete; open the installer URL printed in the server logs")
			return
		}

		switch {
		case strings.HasSuffix(pattern, "/auth/login"),
			strings.HasSuffix(pattern, "/auth/logout"),
			strings.HasSuffix(pattern, "/setup/status"),
			strings.HasSuffix(pattern, "/setup/admin"),
			strings.HasSuffix(pattern, "/public/tickets"):
			// Public: no authentication (setup/admin carries its own token).
			next.ServeHTTP(w, r)
		case strings.HasSuffix(pattern, "/auth/me"):
			requireUser.ServeHTTP(w, r)
		case isPortalPattern(pattern):
			// Customer-only: the hard multi-tenant isolation boundary. An
			// agent/admin principal (including an API key) is refused 403.
			requireCustomer.ServeHTTP(w, r)
		case isCannedReplyPattern(pattern):
			requireAgent.ServeHTTP(w, r)
		case strings.HasSuffix(pattern, "/tags/{id}") && r.Method == http.MethodDelete:
			requireAdmin.ServeHTTP(w, r)
		case isTicketCorePattern(pattern):
			requireAgent.ServeHTTP(w, r)
		case r.Method == http.MethodGet && isAgentReadPattern(pattern):
			requireAgent.ServeHTTP(w, r)
		default:
			// Everything else — users/teams management, the M3 mailbox +
			// settings surface, and the M4 setup instance/complete steps,
			// API keys, and webhooks — is admin only.
			requireAdmin.ServeHTTP(w, r)
		}
	})
}
