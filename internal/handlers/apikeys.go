// Admin CRUD for scoped API keys (M4). The plaintext key is returned exactly
// once, at creation (ApiKeyCreated.key); every later read exposes only the
// display prefix, scopes, and usage metadata — the secret has no read path.
// All operations are admin-only (authPolicy default); the Bearer auth that
// consumes these keys lives in apikey_auth.go.
package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/apikey"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// maxNameLen bounds an operator-supplied display name (API key, webhook,
// canned reply). Shared across the M4 admin handlers.
const maxNameLen = 200

// ListApiKeys returns every key, live first then revoked, newest first.
// Secrets never appear.
func (h *Handlers) ListApiKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListAPIKeys(r.Context())
	if err != nil {
		serverError(w, r, "list api keys", err)
		return
	}
	out := make([]api.ApiKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, listAPIKeyToAPI(row))
	}
	writeJSON(w, r, http.StatusOK, out)
}

// CreateApiKey mints a scoped key and returns the plaintext ONCE. Only the
// SHA-256 digest is persisted.
func (h *Handlers) CreateApiKey(w http.ResponseWriter, r *http.Request) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	var req api.CreateApiKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > maxNameLen {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "name must be 1–200 characters")
		return
	}
	scopes, err := validateAPIKeyScopes(req.Scopes)
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		return
	}
	gen, err := apikey.Generate()
	if err != nil {
		serverError(w, r, "generate api key", err)
		return
	}
	row, err := h.q.CreateAPIKey(r.Context(), store.CreateAPIKeyParams{
		Name:      name,
		KeyPrefix: gen.Prefix,
		KeyHash:   gen.Hash,
		Scopes:    scopes,
		CreatedBy: caller.ID,
	})
	if err != nil {
		serverError(w, r, "create api key", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, api.ApiKeyCreated{
		Id:        row.ID,
		Name:      row.Name,
		KeyPrefix: row.KeyPrefix,
		Key:       gen.Plaintext,
		Scopes:    scopesToAPI(row.Scopes),
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
	})
}

// RevokeApiKey soft-deletes a key: it stops authenticating at once, but the
// row is retained. Idempotent — an unknown or already-revoked key is a 404.
func (h *Handlers) RevokeApiKey(w http.ResponseWriter, r *http.Request, id api.ApiKeyID) {
	n, err := h.q.RevokeAPIKey(r.Context(), id)
	if err != nil {
		serverError(w, r, "revoke api key", err)
		return
	}
	if n == 0 {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such API key, or it is already revoked")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listAPIKeyToAPI is the single store-row -> response conversion; it reads no
// secret column (key_hash is not even selected by ListAPIKeys).
func listAPIKeyToAPI(row store.ListAPIKeysRow) api.ApiKey {
	k := api.ApiKey{
		Id:        row.ID,
		Name:      row.Name,
		KeyPrefix: row.KeyPrefix,
		Scopes:    scopesToAPI(row.Scopes),
		CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt,
	}
	if row.LastUsedAt.Valid {
		t := row.LastUsedAt.Time
		k.LastUsedAt = &t
	}
	if row.RevokedAt.Valid {
		t := row.RevokedAt.Time
		k.RevokedAt = &t
	}
	return k
}

func scopesToAPI(scopes []string) []api.ApiKeyScope {
	out := make([]api.ApiKeyScope, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, api.ApiKeyScope(s))
	}
	return out
}

// validateAPIKeyScopes returns the deduplicated, canonical scope strings, or
// an error if the set is empty or contains an unknown scope.
func validateAPIKeyScopes(in []api.ApiKeyScope) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("at least one scope is required (read and/or write)")
	}
	seen := make(map[api.ApiKeyScope]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		switch s {
		case api.Read, api.Write:
		default:
			return nil, fmt.Errorf("unknown scope %q (allowed: read, write)", s)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, string(s))
	}
	return out, nil
}
