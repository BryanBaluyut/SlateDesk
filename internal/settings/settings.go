// Package settings provides typed access to the settings table and the
// first-boot instance secret bootstrap.
//
// Per architecture doc §4 (Auth), the instance root secret is generated at
// first boot and stored as a plain row in settings: the database is the
// trust root, so encrypting a DB-resident key with a DB-resident key would
// be circular. Every replica reads the same secret with zero config.
package settings

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// KeyInstanceSecret is the settings key holding the base64-encoded 32-byte
// instance root secret (cookie signing; later: mailbox credential key derivation).
const KeyInstanceSecret = "instance_secret"

// KeyExternalURL is the settings key holding the instance's public base URL
// (JSON string, e.g. "https://desk.example.com", no trailing slash; absent
// until configured). It builds the Google OAuth redirect_uri and, later,
// links in outbound email.
const KeyExternalURL = "external_url"

// instanceSecretLen is the secret size in bytes.
const instanceSecretLen = 32

// ErrNotFound is returned by Get when the key does not exist.
var ErrNotFound = errors.New("settings: key not found")

// Store reads and writes typed settings values.
type Store struct {
	q *store.Queries
}

// New returns a settings Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{q: store.New(pool)}
}

// Get unmarshals the JSON value stored under key into dest.
// Returns ErrNotFound if the key does not exist.
func (s *Store) Get(ctx context.Context, key string, dest any) error {
	row, err := s.q.GetSetting(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return fmt.Errorf("settings: get %q: %w", key, err)
	}
	if err := json.Unmarshal(row.Value, dest); err != nil {
		return fmt.Errorf("settings: decode %q: %w", key, err)
	}
	return nil
}

// Set marshals value as JSON and upserts it under key.
func (s *Store) Set(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("settings: encode %q: %w", key, err)
	}
	if err := s.q.SetSetting(ctx, store.SetSettingParams{Key: key, Value: raw}); err != nil {
		return fmt.Errorf("settings: set %q: %w", key, err)
	}
	return nil
}

// Delete removes key. Deleting a missing key is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.q.DeleteSetting(ctx, key); err != nil {
		return fmt.Errorf("settings: delete %q: %w", key, err)
	}
	return nil
}

// EnsureInstanceSecret returns the instance root secret, generating and
// storing a new 32-byte secret on first boot. Safe under concurrent boots:
// the insert is ON CONFLICT DO NOTHING and the stored row always wins.
func (s *Store) EnsureInstanceSecret(ctx context.Context) ([]byte, error) {
	secret, err := s.instanceSecret(ctx)
	if err == nil {
		return secret, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	fresh := make([]byte, instanceSecretLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("settings: generate instance secret: %w", err)
	}
	raw, err := json.Marshal(base64.StdEncoding.EncodeToString(fresh))
	if err != nil {
		return nil, fmt.Errorf("settings: encode instance secret: %w", err)
	}
	if _, err := s.q.SetSettingIfAbsent(ctx, store.SetSettingIfAbsentParams{
		Key:   KeyInstanceSecret,
		Value: raw,
	}); err != nil {
		return nil, fmt.Errorf("settings: store instance secret: %w", err)
	}

	// Re-read: if another replica won the race, its secret is canonical.
	return s.instanceSecret(ctx)
}

// instanceSecret reads and decodes the stored secret.
func (s *Store) instanceSecret(ctx context.Context) ([]byte, error) {
	var encoded string
	if err := s.Get(ctx, KeyInstanceSecret, &encoded); err != nil {
		return nil, err
	}
	secret, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("settings: decode instance secret: %w", err)
	}
	if len(secret) != instanceSecretLen {
		return nil, fmt.Errorf("settings: instance secret has wrong length %d (want %d)", len(secret), instanceSecretLen)
	}
	return secret, nil
}
