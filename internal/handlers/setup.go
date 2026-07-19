// M4 first-run setup wizard: the one-time installer flow that creates the
// first admin, records the instance identity, and seeds a welcome ticket.
//
// The whole flow is unauthenticated at the door — there is no account yet —
// so POST /setup/admin is guarded by a signed installer TOKEN printed to the
// server logs on first boot (never a default credential). The token proves
// the caller can read the logs; once an admin exists it is worthless, because
// createSetupAdmin refuses (409) when an admin already exists. The later
// steps authenticate with the admin session that /setup/admin establishes.
//
// Setup state lives in the settings table (settings.KeySetupCompleted), not
// process memory, so it survives restarts and is shared across replicas. Every
// step is idempotent and short-circuits (409) once setup is complete.
package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// setupTokenTTL bounds the installer token's life. It is reprinted on every
// boot while setup is incomplete, so a generous window costs nothing; the
// real invalidation is "an admin now exists".
const setupTokenTTL = 7 * 24 * time.Hour

// setupTokenMACPrefix domain-separates the installer-token HMAC from session
// cookies and OAuth-state tokens computed under the same instance secret.
const setupTokenMACPrefix = "slatedesk-setup-token:"

// setupFirstAdminLock is the transactional advisory-lock key that serializes
// first-admin creation across replicas, so two concurrent token-bearing
// requests with different emails cannot each insert an admin. Arbitrary but
// fixed; only needs to differ from the migrations advisory lock.
const setupFirstAdminLock int64 = 0x51A7_ED5C_5E70_AD30

// setupTokenPayload is the signed body of an installer token.
type setupTokenPayload struct {
	ExpiresAt int64 `json:"exp"` // unix seconds
}

// GenerateSetupToken mints a signed installer token valid for setupTokenTTL.
// It is called at serve start (when setup is incomplete) to build the
// one-time installer URL printed to the logs.
func GenerateSetupToken(secret []byte) (string, error) {
	payload, err := json.Marshal(setupTokenPayload{ExpiresAt: time.Now().Add(setupTokenTTL).Unix()})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + setupTokenMAC(secret, encoded), nil
}

// verifySetupToken checks an installer token's signature and expiry.
func verifySetupToken(secret []byte, token string) bool {
	encoded, mac, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	if !hmac.Equal([]byte(setupTokenMAC(secret, encoded)), []byte(mac)) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	var p setupTokenPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	return time.Now().Unix() < p.ExpiresAt
}

func setupTokenMAC(secret []byte, encodedPayload string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(setupTokenMACPrefix + encodedPayload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// GetSetupStatus implements GET /setup/status (public). It reports whether
// the installer has finished and whether the installer token is still
// required (true only while no admin exists yet).
func (h *Handlers) GetSetupStatus(w http.ResponseWriter, r *http.Request) {
	completed, err := h.settings.SetupCompleted(r.Context())
	if err != nil {
		serverError(w, r, "read setup status", err)
		return
	}
	adminExists, err := h.q.AdminExists(r.Context())
	if err != nil {
		serverError(w, r, "check admin exists", err)
		return
	}
	writeJSON(w, r, http.StatusOK, api.SetupStatus{
		Completed:  completed,
		NeedsToken: !adminExists,
	})
}

// CreateSetupAdmin implements POST /setup/admin (public, token-gated): the
// installer's first step. It validates the token, refuses once an admin
// exists (409 — the token is spent), creates/promotes the admin, and opens a
// session for it so the remaining steps authenticate as that admin.
func (h *Handlers) CreateSetupAdmin(w http.ResponseWriter, r *http.Request) {
	var req api.CreateSetupAdminJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if !verifySetupToken(h.secret, req.Token) {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "invalid or expired installer token")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		badRequest(w, r, "name is required")
		return
	}
	email, err := auth.NormalizeEmail(string(req.Email))
	if err != nil {
		badRequest(w, r, "invalid email address")
		return
	}
	if err := auth.ValidatePassword(req.Password); err != nil {
		badRequest(w, r, err.Error())
		return
	}
	// The argon2id hash is computed BEFORE the advisory lock below so a slow
	// hash never widens the check-and-create critical section.
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		serverError(w, r, "hash admin password", err)
		return
	}

	// Serialize the "no admin yet → create the admin" check-and-insert so two
	// concurrent token-bearing requests with different emails cannot each
	// insert an admin (UpsertAdmin is ON CONFLICT (email), so distinct emails
	// would otherwise both succeed). A transactional advisory lock makes
	// AdminExists + UpsertAdmin atomic; it releases on commit/rollback.
	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		serverError(w, r, "begin setup admin tx", err)
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := store.New(tx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", setupFirstAdminLock); err != nil {
		serverError(w, r, "lock first-admin creation", err)
		return
	}

	// The token is invalidated the moment an admin exists — by any path
	// (wizard, `slatedesk admin create`, or the headless env bootstrap).
	adminExists, err := q.AdminExists(ctx)
	if err != nil {
		serverError(w, r, "check admin exists", err)
		return
	}
	if adminExists {
		problem.Write(w, r, http.StatusConflict, "Conflict", "an admin already exists")
		return
	}

	u, err := q.UpsertAdmin(ctx, store.UpsertAdminParams{
		Email:        email,
		Name:         name,
		PasswordHash: pgtype.Text{String: hash, Valid: true},
	})
	if err != nil {
		serverError(w, r, "create setup admin", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, r, "commit setup admin", err)
		return
	}

	token, err := auth.SignSession(h.secret, auth.Session{
		UserID:       u.ID,
		TokenVersion: u.TokenVersion,
		ExpiresAt:    time.Now().Add(h.sessionTTL).Unix(),
	})
	if err != nil {
		serverError(w, r, "sign session", err)
		return
	}
	auth.SetSessionCookie(w, r, token, h.sessionTTL, h.cookieSecure)
	writeJSON(w, r, http.StatusOK, toAPIUser(u))
}

// SetSetupInstance implements POST /setup/instance (admin, setup-only): step
// two, recording the instance display name and public external URL.
func (h *Handlers) SetSetupInstance(w http.ResponseWriter, r *http.Request) {
	if h.rejectIfSetupComplete(w, r) {
		return
	}
	var req api.SetSetupInstanceJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		badRequest(w, r, "name is required")
		return
	}
	normalized, ok := normalizeExternalURL(strings.TrimSpace(req.ExternalUrl))
	if !ok {
		badRequest(w, r, "external_url must be an absolute http(s) URL with no path, query, or fragment (e.g. https://desk.example.com)")
		return
	}
	if err := h.settings.SetInstanceName(r.Context(), name); err != nil {
		serverError(w, r, "store instance name", err)
		return
	}
	if err := h.settings.SetExternalURL(r.Context(), normalized); err != nil {
		serverError(w, r, "store external url", err)
		return
	}
	writeJSON(w, r, http.StatusOK, api.InstanceSettings{Name: name, ExternalUrl: normalized})
}

// CompleteSetup implements POST /setup/complete (admin, setup-only): the
// final step. It marks setup complete — permanently closing the installer —
// and seeds a welcome ticket so the workspace opens on a working example.
func (h *Handlers) CompleteSetup(w http.ResponseWriter, r *http.Request) {
	if h.rejectIfSetupComplete(w, r) {
		return
	}
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired credentials")
		return
	}

	number, err := h.seedWelcomeTicket(r.Context(), caller.ID)
	if err != nil {
		serverError(w, r, "seed welcome ticket", err)
		return
	}
	if err := h.settings.SetSetupCompleted(r.Context(), true); err != nil {
		serverError(w, r, "mark setup complete", err)
		return
	}
	h.setupCompleted.Store(true)

	res := api.SetupCompleteResult{Completed: true}
	if number != "" {
		res.WelcomeTicketNumber = &number
	}
	writeJSON(w, r, http.StatusOK, res)
}

// rejectIfSetupComplete writes a 409 and returns true when setup has already
// finished (the setup steps are one-shot).
func (h *Handlers) rejectIfSetupComplete(w http.ResponseWriter, r *http.Request) bool {
	done, err := h.settings.SetupCompleted(r.Context())
	if err != nil {
		serverError(w, r, "read setup status", err)
		return true
	}
	if done {
		problem.Write(w, r, http.StatusConflict, "Conflict", "setup is already complete")
		return true
	}
	return false
}

// seedWelcomeTicket creates the first-run example ticket, requested by the
// admin who completed the wizard.
func (h *Handlers) seedWelcomeTicket(ctx context.Context, adminID uuid.UUID) (string, error) {
	return SeedWelcomeTicket(ctx, h.svc, h.q, adminID)
}

// SeedWelcomeTicket creates the first-run example ticket, requested by
// requesterID, with a single system-authored article. It is a no-op
// (returning "") when any ticket already exists, so re-running against a
// populated instance never duplicates it. Shared by the wizard's
// CompleteSetup and the headless boot path (announceSetup), so a headless/IaC
// install also lands on a working example rather than an empty table
// (architecture doc §5 step 4).
func SeedWelcomeTicket(ctx context.Context, svc *ticket.Service, q *store.Queries, requesterID uuid.UUID) (string, error) {
	existing, err := q.ListTickets(ctx, store.ListTicketsParams{PageLimit: 1})
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return "", nil
	}

	created, _, err := svc.CreateTicket(ctx, ticket.CreateTicketParams{
		Subject:     "Welcome to SlateDesk",
		RequesterID: requesterID,
		Article: ticket.ArticleInput{
			SenderType: store.ArticleSenderSystem,
			Channel:    store.ArticleChannelWeb,
			BodyText: "Welcome to SlateDesk — your help desk is ready.\n\n" +
				"This is a sample ticket. Reply to try the composer, then close " +
				"it when you are done. New tickets arrive here from email, the " +
				"public web form, the customer portal, and the API.",
		},
	})
	if err != nil {
		return "", err
	}
	return created.Number, nil
}
