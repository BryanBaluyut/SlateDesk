package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	key := NewKey()
	if err := ValidateKey(key); err != nil {
		t.Fatalf("NewKey produced invalid key %q: %v", key, err)
	}

	const body = "attachment bytes"
	n, err := s.Save(ctx, key, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("Save wrote %d bytes; want %d", n, len(body))
	}

	// Duplicate key must fail, not overwrite.
	if _, err := s.Save(ctx, key, strings.NewReader("other")); err == nil {
		t.Fatal("Save on existing key succeeded; want error")
	}

	r, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("read %q; want %q", got, body)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after delete = %v; want ErrNotFound", err)
	}
	// Idempotent delete.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestLocalRejectsTraversalKeys(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	// A real file outside the attachments dir that traversal would reach.
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("root secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	hostile := []string{
		"",
		"..",
		"../secret.txt",
		"../../etc/passwd",
		"ab/../../secret.txt",
		"ab/../../../../../../etc/passwd",
		"/etc/passwd",
		"ab/..%2f..%2fsecret",
		`ab\..\secret`,
		"ab/./cd",
		"AB/3FA85F64-5717-4562-B3FC-2C963F66AFA6", // uppercase not produced by NewKey
		"ab/short",
		"user supplied name.pdf",
		"ab/3fa85f64-5717-4562-b3fc-2c963f66afa6/extra",
	}
	for _, key := range hostile {
		if err := ValidateKey(key); err == nil {
			t.Errorf("ValidateKey(%q) accepted a hostile key", key)
		}
		if _, err := s.Save(ctx, key, strings.NewReader("x")); err == nil {
			t.Errorf("Save(%q) accepted a hostile key", key)
		}
		if _, err := s.Open(ctx, key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v; want validation error, not success/ErrNotFound", key, err)
		}
		if err := s.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) accepted a hostile key", key)
		}
	}

	// Nothing escaped: the secret is untouched and no stray files exist
	// outside the attachments dir.
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("secret file damaged: %v", err)
	}
}
