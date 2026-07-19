package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BryanBaluyut/slatedesk/internal/apikey"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// createTestUser inserts a user with the given role and returns it.
func createTestUser(t *testing.T, q *store.Queries, role store.UserRole) store.User {
	t.Helper()
	u, err := q.CreateUser(context.Background(), store.CreateUserParams{
		Email: fmt.Sprintf("u-%s@slatedesk.test", uuid.NewString()[:8]),
		Name:  "Test User",
		Role:  role,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// TestAPIKeyLifecycle exercises the create -> authenticate -> touch -> revoke
// path, with emphasis on GetAPIKeyForAuth: it must resolve a key's hash to the
// minting user's identity and the key's scopes, and must stop matching once
// the key is revoked (the machine-auth security boundary).
func TestAPIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	admin := createTestUser(t, q, store.UserRoleAdmin)

	gen, err := apikey.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	created, err := q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name:      "ci-runner",
		KeyPrefix: gen.Prefix,
		KeyHash:   gen.Hash,
		Scopes:    []string{"read", "write"},
		CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if created.RevokedAt.Valid {
		t.Fatalf("new key came back revoked")
	}

	// Authenticate by the presented key's hash: identity + scopes resolve.
	row, err := q.GetAPIKeyForAuth(ctx, apikey.Hash(gen.Plaintext))
	if err != nil {
		t.Fatalf("auth lookup: %v", err)
	}
	if row.UserID != admin.ID {
		t.Errorf("auth resolved user %s, want minting admin %s", row.UserID, admin.ID)
	}
	if row.UserRole != store.UserRoleAdmin {
		t.Errorf("auth user role = %q, want admin", row.UserRole)
	}
	if !row.UserActive {
		t.Errorf("auth user should be active")
	}
	if row.ApiKeyID != created.ID {
		t.Errorf("auth key id = %s, want %s", row.ApiKeyID, created.ID)
	}
	if len(row.Scopes) != 2 || row.Scopes[0] != "read" || row.Scopes[1] != "write" {
		t.Errorf("auth scopes = %v, want [read write]", row.Scopes)
	}

	// A wrong hash resolves to nothing.
	if _, err := q.GetAPIKeyForAuth(ctx, apikey.Hash("sd_live_not-a-real-key")); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("wrong-hash lookup err = %v, want ErrNoRows", err)
	}

	// Touch stamps last_used_at without disturbing anything else.
	if err := q.TouchAPIKey(ctx, created.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	after, err := q.GetAPIKey(ctx, created.ID)
	if err != nil {
		t.Fatalf("get api key: %v", err)
	}
	if !after.LastUsedAt.Valid {
		t.Errorf("last_used_at not set after touch")
	}

	// Revoke is idempotent and immediately closes the auth path.
	n, err := q.RevokeAPIKey(ctx, created.ID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if n != 1 {
		t.Errorf("first revoke affected %d rows, want 1", n)
	}
	if _, err := q.GetAPIKeyForAuth(ctx, gen.Hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("revoked key still authenticates (err=%v)", err)
	}
	n, err = q.RevokeAPIKey(ctx, created.ID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if n != 0 {
		t.Errorf("second revoke affected %d rows, want 0 (already revoked)", n)
	}

	// The revoked key still appears in the admin listing (history), without
	// exposing its hash.
	list, err := q.ListAPIKeys(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	var seen bool
	for _, k := range list {
		if k.ID == created.ID {
			seen = true
			if !k.RevokedAt.Valid {
				t.Errorf("listed key should show as revoked")
			}
		}
	}
	if !seen {
		t.Errorf("revoked key missing from listing")
	}
}
