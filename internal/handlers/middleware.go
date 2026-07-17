package handlers

import (
	"context"
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

// RequireUser authenticates the request from the sd_session cookie: verify
// the HMAC signature and expiry, load the user, and compare token_version
// (architecture doc §4 — the per-request user fetch makes logout-everywhere
// free). On success the user is stored in the request context; otherwise a
// 401 problem is written. There is deliberately no detail in the 401s: the
// caller learns only that the session is not valid.
func (h *Handlers) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unauthorized := func() {
			problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		}

		c, err := r.Cookie(auth.SessionCookieName)
		if err != nil || c.Value == "" {
			unauthorized()
			return
		}
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
	})
}

// RequireRole allows only authenticated users with the given role. It must
// run after RequireUser (no user in context reads as a 401, not a 500).
func RequireRole(role api.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, ok := CurrentUser(r.Context())
			if !ok {
				problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
				return
			}
			if api.Role(u.Role) != role {
				problem.Write(w, r, http.StatusForbidden, "Forbidden", "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authPolicy maps each operation to its access rule, deny-by-default:
//
//   - /auth/login, /auth/logout: public (spec: security [])
//   - /auth/me:                  any authenticated user
//   - everything else:           admin only (M1: users/teams management)
//
// It runs inside the generated route wrapper, so the chi route pattern is
// already resolved; matching on the pattern suffix keeps the policy
// independent of the mount point (/api/v1).
func (h *Handlers) authPolicy(next http.Handler) http.Handler {
	requireUser := h.RequireUser(next)
	requireAdmin := h.RequireUser(RequireRole(api.Admin)(next))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := chi.RouteContext(r.Context()).RoutePattern()
		switch {
		case strings.HasSuffix(pattern, "/auth/login"),
			strings.HasSuffix(pattern, "/auth/logout"):
			next.ServeHTTP(w, r)
		case strings.HasSuffix(pattern, "/auth/me"):
			requireUser.ServeHTTP(w, r)
		default:
			requireAdmin.ServeHTTP(w, r)
		}
	})
}
