// Machine authentication for /api/v1: an Authorization: Bearer sd_live_… key
// is accepted as an ALTERNATIVE to the sd_session cookie on every operation
// (architecture doc §4 — "scoped API keys, Bearer-only"). The two are never
// both required: RequireUser (middleware.go) tries the cookie first and falls
// back to the Bearer header, so a request carrying either alone authenticates.
//
// A key request acts as the admin or agent who minted it (role gates apply
// unchanged) and additionally passes a scope gate: safe methods need the
// `read` scope, mutating methods need `write`. Scopes cannot be expressed on
// an HTTP-bearer OpenAPI scheme, so the gate is enforced here and documented
// in api/openapi.yaml.
package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/apikey"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// apiKeyTouchTimeout bounds the best-effort last_used_at stamp fired off the
// hot auth path.
const apiKeyTouchTimeout = 5 * time.Second

// errInvalidAPIKey marks an authentication failure the caller maps to a 401
// (unknown, revoked, malformed key, or an inactive owner) — as opposed to a
// transient database error, which must surface as a 500.
var errInvalidAPIKey = errors.New("handlers: invalid api key")

// apiKeyPrincipal is the resolved identity behind a presented key: the acting
// user, the key's id (for the usage stamp), and its scopes (for the gate).
type apiKeyPrincipal struct {
	user   store.User
	keyID  uuid.UUID
	scopes []string
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. ok is true only when the scheme is Bearer (case-insensitive) and a
// non-empty token follows; anything else (no header, another scheme, empty
// token) returns false so RequireUser falls through to cookie auth.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}

// authenticateAPIKey resolves a presented plaintext key to its principal. It
// returns errInvalidAPIKey for anything that should read as 401 (bad format,
// no such live key, deactivated owner) and a wrapped database error otherwise.
// last_used_at is NOT touched here (kept off the read path); the caller stamps
// it asynchronously after the scope gate passes.
func (h *Handlers) authenticateAPIKey(ctx context.Context, token string) (apiKeyPrincipal, error) {
	if !apikey.HasValidFormat(token) {
		return apiKeyPrincipal{}, errInvalidAPIKey
	}
	row, err := h.q.GetAPIKeyForAuth(ctx, apikey.Hash(token))
	if err != nil {
		if isNoRows(err) {
			return apiKeyPrincipal{}, errInvalidAPIKey
		}
		return apiKeyPrincipal{}, err
	}
	if !row.UserActive {
		return apiKeyPrincipal{}, errInvalidAPIKey
	}
	return apiKeyPrincipal{
		user: store.User{
			ID:           row.UserID,
			Email:        row.UserEmail,
			Name:         row.UserName,
			Role:         row.UserRole,
			TokenVersion: row.UserTokenVersion,
			Active:       row.UserActive,
		},
		keyID:  row.ApiKeyID,
		scopes: row.Scopes,
	}, nil
}

// touchAPIKeyAsync stamps last_used_at without blocking the request. Failures
// are logged, never surfaced — a usage timestamp is not worth a 500.
func (h *Handlers) touchAPIKeyAsync(id uuid.UUID) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), apiKeyTouchTimeout)
		defer cancel()
		if err := h.q.TouchAPIKey(ctx, id); err != nil {
			slog.Warn("api key: touch last_used_at", "key_id", id, "error", err)
		}
	}()
}

// enforceAPIKeyScope gates a key request by HTTP method: safe methods need
// `read` (write implies read); mutating methods need `write`. It writes a 403
// problem and returns false when the scope is insufficient.
func enforceAPIKeyScope(w http.ResponseWriter, r *http.Request, scopes []string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		if hasScope(scopes, string(api.Read)) || hasScope(scopes, string(api.Write)) {
			return true
		}
		problem.Write(w, r, http.StatusForbidden, "Forbidden", "this API key lacks the 'read' scope")
		return false
	default:
		if hasScope(scopes, string(api.Write)) {
			return true
		}
		problem.Write(w, r, http.StatusForbidden, "Forbidden", "this API key lacks the 'write' scope")
		return false
	}
}

func hasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
