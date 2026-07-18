// Package storage abstracts attachment blob storage behind a small
// interface (architecture doc §4: local | S3). M2 ships the local-disk
// implementation; an S3 implementation slots in at M5 without touching
// callers.
//
// Keys are opaque, storage-generated identifiers (see NewKey) — never
// user-supplied filenames — and are validated on every operation, so a
// hostile key can never escape the storage root.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ErrNotFound is returned by Open when no blob exists under the key.
var ErrNotFound = errors.New("storage: not found")

// Storage stores and retrieves attachment blobs by key.
type Storage interface {
	// Save streams r to the blob identified by key and returns the number
	// of bytes written. Keys must come from NewKey; saving to an existing
	// key is an error (keys are single-use by construction).
	Save(ctx context.Context, key string, r io.Reader) (int64, error)

	// Open returns a reader for the blob. The caller must Close it.
	// Returns an error wrapping ErrNotFound if the key does not exist.
	Open(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the blob. Deleting a missing key is not an error
	// (delete is idempotent).
	Delete(ctx context.Context, key string) error
}

// NewKey returns a fresh storage key: a UUID sharded under a two-character
// prefix directory (e.g. "3f/3fa85f64-5717-4562-b3fc-2c963f66afa6") to keep
// directory fan-out sane on local disk. Keys never contain user input.
func NewKey() string {
	id := uuid.NewString()
	return id[:2] + "/" + id
}

// keyPattern is deliberately strict: lowercase hex/dash path segments as
// produced by NewKey. No dots, so ".." cannot occur; no leading slash, so
// keys are always relative.
var keyPattern = regexp.MustCompile(`^[0-9a-f]{2}/[0-9a-f-]{36}$`)

// ValidateKey rejects any key that NewKey could not have produced. It is
// exported so callers (and tests) can check keys coming out of the
// database before touching storage.
func ValidateKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("storage: invalid key %q", key)
	}
	// Defense in depth: the pattern above already excludes traversal, but
	// verify the resulting relative path stays local anyway.
	if strings.Contains(key, "..") || !filepath.IsLocal(filepath.FromSlash(key)) {
		return fmt.Errorf("storage: invalid key %q", key)
	}
	return nil
}
