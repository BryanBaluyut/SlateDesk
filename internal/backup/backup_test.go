package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the pure archive-handling paths (manifest validation,
// attachment packing/unpacking, the path-traversal guard). The database
// halves shell out to pg_dump/psql and are proven by the documented live
// round-trip against a real Postgres — they are intentionally not unit
// tested here so the suite never depends on a matching pg client version.

func TestCheckManifest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"current", fmt.Sprintf("%s\nformat: %d\ncreated: now\n", manifestID, FormatVersion), false},
		{"older format still readable", manifestID + "\nformat: 0\n", false},
		{"wrong id", "Not A Backup\nformat: 1\n", true},
		{"newer format rejected", fmt.Sprintf("%s\nformat: %d\n", manifestID, FormatVersion+1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkManifest(strings.NewReader(tc.body))
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRestoreAttachmentTraversalGuard(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bad := []string{
		attachPrefix + "../escape.bin",
		attachPrefix + "../../etc/passwd",
	}
	for _, name := range bad {
		hdr := &tar.Header{Name: name, Size: 3}
		if err := restoreAttachment(dir, hdr, strings.NewReader("xxx")); err == nil {
			t.Fatalf("expected traversal %q to be rejected", name)
		}
	}
	// A nested-but-safe path is written.
	hdr := &tar.Header{Name: attachPrefix + "ab/ok.bin", Size: 2}
	if err := restoreAttachment(dir, hdr, strings.NewReader("hi")); err != nil {
		t.Fatalf("safe path rejected: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ab", "ok.bin"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("safe attachment not written correctly: got %q err %v", got, err)
	}
}

func TestAttachmentsPackUnpackRoundTrip(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	files := map[string]string{
		"top.bin":        "top-level blob",
		"ab/one.bin":     "nested one",
		"ab/cd/two.bin":  "deeper two",
		"empty/keep.bin": "",
	}
	for rel, content := range files {
		full := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Pack into an in-memory .tar.gz using the same helpers Backup uses.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := writeTarBytes(tw, manifestName, []byte(manifestID+"\nformat: 1\n")); err != nil {
		t.Fatal(err)
	}
	if err := addAttachments(context.Background(), tw, filepath.Join(src, "attachments")); err != nil {
		t.Fatalf("addAttachments on missing dir should be a no-op: %v", err)
	}
	// Now pack a real attachments dir.
	if err := addAttachments(context.Background(), tw, src); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	// Unpack into a fresh dir via the tar reader + restoreAttachment.
	dst := t.TempDir()
	gzr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gzr)
	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(hdr.Name, attachPrefix) {
			continue
		}
		if err := restoreAttachment(filepath.Join(dst, "attachments"), hdr, tr); err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != len(files) {
		t.Fatalf("expected %d attachment members, got %d", len(files), count)
	}
	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dst, "attachments", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("missing restored %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("content mismatch for %s: got %q want %q", rel, got, want)
		}
	}
}

func TestBackupRequiresDatabaseURL(t *testing.T) {
	t.Parallel()
	if err := Backup(context.Background(), io.Discard, Options{}); err == nil {
		t.Fatal("expected error when DATABASE_URL is empty")
	}
	if err := Restore(context.Background(), bytes.NewReader(nil), Options{}); err == nil {
		t.Fatal("expected error when DATABASE_URL is empty")
	}
}
