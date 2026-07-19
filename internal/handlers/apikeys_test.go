package handlers_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/apikey"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// seedKeyRow mints a key and stores it directly (bypassing the admin-only
// create endpoint) so tests can control the creator's role and scope set —
// the two things the Bearer auth path keys off. Returns the one-time plaintext
// and the row id.
func (e *env) seedKeyRow(createdBy uuid.UUID, scopes ...string) (string, uuid.UUID) {
	e.t.Helper()
	gen, err := apikey.Generate()
	if err != nil {
		e.t.Fatalf("generate api key: %v", err)
	}
	row, err := e.q.CreateAPIKey(context.Background(), store.CreateAPIKeyParams{
		Name:      uniqueName("key"),
		KeyPrefix: gen.Prefix,
		KeyHash:   gen.Hash,
		Scopes:    scopes,
		CreatedBy: createdBy,
	})
	if err != nil {
		e.t.Fatalf("seed api key: %v", err)
	}
	return gen.Plaintext, row.ID
}

func bearer(token string) [2]string { return [2]string{"Authorization", "Bearer " + token} }

// TestAPIKeyBearerAuthMatrix is the core machine-auth contract: a key
// authenticates as its creator (role gates apply), a read key is refused on
// writes, and revoked/wrong/malformed keys are 401.
func TestAPIKeyBearerAuthMatrix(t *testing.T) {
	e := newEnv(t)
	agent := e.seedUser(store.UserRoleAgent)
	admin := e.seedUser(store.UserRoleAdmin)

	// A ticket-core read (agent surface) and a ticket-core write (POST /tags).
	const readPath = "/api/v1/tickets"
	const writePath = "/api/v1/tags"
	tagBody := map[string]string{"name": uniqueName("tag"), "color": "#3366ff"}

	t.Run("read+write agent key", func(t *testing.T) {
		key, _ := e.seedKeyRow(agent.ID, "read", "write")
		if res := e.do(http.MethodGet, readPath, nil, nil, bearer(key)); res.status != http.StatusOK {
			t.Fatalf("GET with read+write key = %d, want 200; body %s", res.status, res.body)
		}
		if res := e.do(http.MethodPost, writePath, nil, tagBody, bearer(key)); res.status != http.StatusCreated {
			t.Fatalf("POST with write key = %d, want 201; body %s", res.status, res.body)
		}
	})

	t.Run("read-only key denied on write", func(t *testing.T) {
		key, _ := e.seedKeyRow(agent.ID, "read")
		if res := e.do(http.MethodGet, readPath, nil, nil, bearer(key)); res.status != http.StatusOK {
			t.Fatalf("GET with read key = %d, want 200", res.status)
		}
		res := e.do(http.MethodPost, writePath, nil, tagBody, bearer(key))
		if res.status != http.StatusForbidden {
			t.Fatalf("POST with read-only key = %d, want 403; body %s", res.status, res.body)
		}
	})

	t.Run("agent key cannot reach admin surface", func(t *testing.T) {
		key, _ := e.seedKeyRow(agent.ID, "read", "write")
		// /api-keys is admin-only; an agent-owned key must be 403, not 200.
		if res := e.do(http.MethodGet, "/api/v1/api-keys", nil, nil, bearer(key)); res.status != http.StatusForbidden {
			t.Fatalf("agent key GET /api-keys = %d, want 403; body %s", res.status, res.body)
		}
	})

	t.Run("admin key reaches admin surface", func(t *testing.T) {
		key, _ := e.seedKeyRow(admin.ID, "read", "write")
		if res := e.do(http.MethodGet, "/api/v1/api-keys", nil, nil, bearer(key)); res.status != http.StatusOK {
			t.Fatalf("admin key GET /api-keys = %d, want 200; body %s", res.status, res.body)
		}
	})

	t.Run("revoked key is 401", func(t *testing.T) {
		key, id := e.seedKeyRow(agent.ID, "read", "write")
		if _, err := e.q.RevokeAPIKey(context.Background(), id); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if res := e.do(http.MethodGet, readPath, nil, nil, bearer(key)); res.status != http.StatusUnauthorized {
			t.Fatalf("revoked key = %d, want 401", res.status)
		}
	})

	t.Run("inactive owner is 401", func(t *testing.T) {
		u := e.seedUser(store.UserRoleAgent)
		key, _ := e.seedKeyRow(u.ID, "read", "write")
		if err := e.q.DeactivateUser(context.Background(), u.ID); err != nil {
			t.Fatalf("deactivate owner: %v", err)
		}
		if res := e.do(http.MethodGet, readPath, nil, nil, bearer(key)); res.status != http.StatusUnauthorized {
			t.Fatalf("inactive-owner key = %d, want 401", res.status)
		}
	})

	t.Run("wrong and malformed keys are 401", func(t *testing.T) {
		other, err := apikey.Generate() // valid format, never stored
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		for name, tok := range map[string]string{
			"unknown-valid-format": other.Plaintext,
			"malformed":            "sd_live_not-a-real-key",
			"garbage":              "hello",
			"empty":                "",
		} {
			res := e.do(http.MethodGet, readPath, nil, nil, bearer(tok))
			if res.status != http.StatusUnauthorized {
				t.Errorf("%s: status = %d, want 401", name, res.status)
			}
		}
	})
}

// TestAPIKeyCRUDAndSecretEgress covers the admin CRUD surface: the plaintext
// is returned once at creation and never again, and only admins may manage
// keys.
func TestAPIKeyCRUDAndSecretEgress(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	agent := e.seedUser(store.UserRoleAgent)
	customer := e.seedUser(store.UserRoleCustomer)
	adminCookie := e.mustLogin(admin.Email, seedPassword)

	// Create returns the plaintext exactly once.
	create := e.do(http.MethodPost, "/api/v1/api-keys", adminCookie, map[string]any{
		"name":   "CI deploy key",
		"scopes": []string{"read", "write"},
	})
	if create.status != http.StatusCreated {
		t.Fatalf("create key = %d, want 201; body %s", create.status, create.body)
	}
	var created struct {
		Id        string   `json:"id"`
		Key       string   `json:"key"`
		KeyPrefix string   `json:"key_prefix"`
		Scopes    []string `json:"scopes"`
	}
	e.decode(create, &created)
	if !strings.HasPrefix(created.Key, apikey.Prefix) {
		t.Fatalf("returned key %q missing %q prefix", created.Key, apikey.Prefix)
	}
	if !apikey.HasValidFormat(created.Key) {
		t.Fatalf("returned key is not a valid key: %q", created.Key)
	}
	plaintext := created.Key

	// The plaintext authenticates.
	if res := e.do(http.MethodGet, "/api/v1/api-keys", nil, nil, bearer(plaintext)); res.status != http.StatusOK {
		t.Fatalf("created key does not authenticate: %d", res.status)
	}

	// List never re-exposes the secret (no key / key_hash fields; plaintext
	// absent from the whole body).
	list := e.do(http.MethodGet, "/api/v1/api-keys", adminCookie, nil)
	if list.status != http.StatusOK {
		t.Fatalf("list keys = %d", list.status)
	}
	if strings.Contains(string(list.body), plaintext) {
		t.Fatalf("list body leaked the plaintext key")
	}
	var rows []map[string]any
	e.decode(list, &rows)
	if len(rows) == 0 {
		t.Fatal("list returned no keys")
	}
	for _, row := range rows {
		if _, ok := row["key"]; ok {
			t.Errorf("list element exposes plaintext 'key'")
		}
		if _, ok := row["key_hash"]; ok {
			t.Errorf("list element exposes 'key_hash'")
		}
	}

	// Non-admins may not manage keys.
	agentCookie := e.mustLogin(agent.Email, seedPassword)
	customerCookie := e.mustLogin(customer.Email, seedPassword)
	if res := e.do(http.MethodGet, "/api/v1/api-keys", agentCookie, nil); res.status != http.StatusForbidden {
		t.Errorf("agent list keys = %d, want 403", res.status)
	}
	if res := e.do(http.MethodPost, "/api/v1/api-keys", agentCookie, map[string]any{"name": "x", "scopes": []string{"read"}}); res.status != http.StatusForbidden {
		t.Errorf("agent create key = %d, want 403", res.status)
	}
	if res := e.do(http.MethodGet, "/api/v1/api-keys", customerCookie, nil); res.status != http.StatusForbidden {
		t.Errorf("customer list keys = %d, want 403", res.status)
	}
	if res := e.do(http.MethodGet, "/api/v1/api-keys", nil, nil); res.status != http.StatusUnauthorized {
		t.Errorf("anon list keys = %d, want 401", res.status)
	}

	// Revoke is idempotent-ish: 204 then 404, and the key stops authenticating.
	del := e.do(http.MethodDelete, "/api/v1/api-keys/"+created.Id, adminCookie, nil)
	if del.status != http.StatusNoContent {
		t.Fatalf("revoke key = %d, want 204; body %s", del.status, del.body)
	}
	if res := e.do(http.MethodDelete, "/api/v1/api-keys/"+created.Id, adminCookie, nil); res.status != http.StatusNotFound {
		t.Errorf("re-revoke = %d, want 404", res.status)
	}
	if res := e.do(http.MethodGet, "/api/v1/api-keys", nil, nil, bearer(plaintext)); res.status != http.StatusUnauthorized {
		t.Errorf("revoked key still authenticates: %d", res.status)
	}
}

// TestCreateAPIKeyValidation rejects empty names and bad/empty scopes.
func TestCreateAPIKeyValidation(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)

	cases := []map[string]any{
		{"name": "", "scopes": []string{"read"}},
		{"name": "ok", "scopes": []string{}},
		{"name": "ok", "scopes": []string{"admin"}},
		{"name": "ok", "scopes": []string{"read", "delete"}},
	}
	for i, body := range cases {
		if res := e.do(http.MethodPost, "/api/v1/api-keys", cookie, body); res.status != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400; body %s", i, res.status, res.body)
		}
	}
}
