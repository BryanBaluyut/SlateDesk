package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultDataDir is used when SLATEDESK_DATA_DIR is unset.
const DefaultDataDir = "./data"

// DataDirFromEnv resolves the local storage root: SLATEDESK_DATA_DIR, or
// DefaultDataDir when unset. Attachments live under <root>/attachments.
func DataDirFromEnv() string {
	if dir := os.Getenv("SLATEDESK_DATA_DIR"); dir != "" {
		return dir
	}
	return DefaultDataDir
}

// Local is the local-disk Storage implementation. Blobs live at
// <root>/attachments/<key>. All keys are validated (see ValidateKey), so no
// operation can address a path outside the root.
type Local struct {
	root string
}

// NewLocal creates the storage root (root/attachments) if needed and
// returns a Local storage.
func NewLocal(root string) (*Local, error) {
	if root == "" {
		return nil, fmt.Errorf("storage: root directory required")
	}
	dir := filepath.Join(root, "attachments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("storage: create root %s: %w", dir, err)
	}
	return &Local{root: dir}, nil
}

// path maps a validated key to its on-disk location.
func (l *Local) path(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

// Save implements Storage. The file is created exclusively (a duplicate key
// is a bug, not an overwrite) and removed again on a partial write.
func (l *Local) Save(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("storage: save %s: %w", key, err)
	}
	path, err := l.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, fmt.Errorf("storage: create shard dir for %s: %w", key, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("storage: create %s: %w", key, err)
	}
	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return 0, fmt.Errorf("storage: write %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return 0, fmt.Errorf("storage: close %s: %w", key, err)
	}
	return n, nil
}

// Open implements Storage.
func (l *Local) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", key, err)
	}
	path, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("storage: open %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("storage: open %s: %w", key, err)
	}
	return f, nil
}

// Delete implements Storage. Missing keys are ignored (idempotent).
func (l *Local) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: delete %s: %w", key, err)
	}
	path, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete %s: %w", key, err)
	}
	return nil
}
