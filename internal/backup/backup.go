// Package backup implements `slatedesk backup` and `slatedesk restore`:
// a single-file, cron-friendly snapshot of a SlateDesk instance.
//
// A backup is a gzip-compressed tar containing three kinds of member:
//
//	MANIFEST         a few lines of plain-text metadata (format + timestamp)
//	database.sql     a plain-format pg_dump of the whole database
//	attachments/…    every blob under <SLATEDESK_DATA_DIR>/attachments
//
// The database half shells out to the standard PostgreSQL client tools
// (pg_dump / psql): a backup is exactly what an operator would take by hand,
// stays inspectable, and restores with vanilla tooling if the binary is ever
// unavailable. Those tools are NOT in the distroless app image, so backups
// run from the host binary or a container that has postgresql-client — see
// docs/deploy.md. The client major version should be >= the server's.
//
// The whole archive streams: pg_dump goes to a temp file (so the tar header
// can carry an exact size), attachments stream straight off disk, and the
// result is written to a path or to stdout (`slatedesk backup > out.tar.gz`).
package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// FormatVersion is bumped only on an incompatible archive-layout change.
	FormatVersion = 1

	manifestName = "MANIFEST"
	dumpName     = "database.sql"
	attachPrefix = "attachments/"
	manifestID   = "SlateDesk backup"
)

// Options configures a backup or restore.
type Options struct {
	// DatabaseURL is the Postgres connection string passed to pg_dump/psql.
	DatabaseURL string
	// DataDir is the storage root (SLATEDESK_DATA_DIR); attachments live
	// under DataDir/attachments.
	DataDir string
	// Force, on restore, permits overwriting a non-empty database.
	Force bool
}

// attachmentsDir returns the directory holding attachment blobs.
func (o Options) attachmentsDir() string { return filepath.Join(o.DataDir, "attachments") }

// Backup writes a .tar.gz snapshot (database + attachments) to w.
func Backup(ctx context.Context, w io.Writer, opts Options) error {
	if opts.DatabaseURL == "" {
		return fmt.Errorf("backup: DATABASE_URL is required")
	}
	pgDump, err := exec.LookPath("pg_dump")
	if err != nil {
		return fmt.Errorf("backup: pg_dump not found on PATH — install the postgresql-client package (major version >= the server): %w", err)
	}

	// pg_dump -> temp file, so the tar header can declare an exact size
	// without buffering the whole dump in memory.
	dumpFile, err := os.CreateTemp("", "slatedesk-dump-*.sql")
	if err != nil {
		return fmt.Errorf("backup: create temp dump: %w", err)
	}
	defer os.Remove(dumpFile.Name())
	defer dumpFile.Close()

	var dumpErr bytes.Buffer
	cmd := exec.CommandContext(ctx, pgDump,
		"--no-owner", "--no-privileges", "--format=plain",
		"--dbname="+opts.DatabaseURL)
	cmd.Stdout = dumpFile
	cmd.Stderr = &dumpErr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("backup: pg_dump failed: %w: %s", err, strings.TrimSpace(dumpErr.String()))
	}
	dumpSize, err := dumpFile.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("backup: size dump: %w", err)
	}
	if _, err := dumpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("backup: rewind dump: %w", err)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	manifest := fmt.Sprintf("%s\nformat: %d\ncreated: %s\n",
		manifestID, FormatVersion, time.Now().UTC().Format(time.RFC3339))
	if err := writeTarBytes(tw, manifestName, []byte(manifest)); err != nil {
		return err
	}
	if err := writeTarFile(tw, dumpName, dumpFile, dumpSize); err != nil {
		return fmt.Errorf("backup: write dump into archive: %w", err)
	}
	if err := addAttachments(ctx, tw, opts.attachmentsDir()); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: finalize tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backup: finalize gzip: %w", err)
	}
	return nil
}

// addAttachments streams every regular file under dir into the archive under
// the attachments/ prefix. A missing directory is not an error — an instance
// may simply have no attachments yet.
func addAttachments(ctx context.Context, tw *tar.Writer, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("backup: stat attachments dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup: attachments path %s is not a directory", dir)
	}
	return filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("backup: walk attachments: %w", err)
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("backup: relativize %s: %w", path, err)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("backup: open attachment %s: %w", path, err)
		}
		defer f.Close()
		name := attachPrefix + filepath.ToSlash(rel)
		return writeTarFile(tw, name, f, fi.Size())
	})
}

// writeTarBytes writes an in-memory member.
func writeTarBytes(tw *tar.Writer, name string, b []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: tar header %s: %w", name, err)
	}
	if _, err := tw.Write(b); err != nil {
		return fmt.Errorf("backup: tar body %s: %w", name, err)
	}
	return nil
}

// writeTarFile streams a member of known size from r.
func writeTarFile(tw *tar.Writer, name string, r io.Reader, size int64) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: size, ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: tar header %s: %w", name, err)
	}
	if _, err := io.Copy(tw, r); err != nil {
		return fmt.Errorf("backup: tar body %s: %w", name, err)
	}
	return nil
}

// Restore rebuilds an instance from a .tar.gz produced by Backup. Unless
// opts.Force is set it refuses to run against a database that already holds
// users, so a stray restore cannot clobber a live instance. The public
// schema is dropped and recreated before the dump is applied, so a restore
// is deterministic whether the target is empty or already migrated.
//
// The app should not be serving during a restore (document + DROP SCHEMA
// need exclusive-ish access). Attachments are written under DataDir.
func Restore(ctx context.Context, r io.Reader, opts Options) error {
	if opts.DatabaseURL == "" {
		return fmt.Errorf("restore: DATABASE_URL is required")
	}
	psql, err := exec.LookPath("psql")
	if err != nil {
		return fmt.Errorf("restore: psql not found on PATH — install the postgresql-client package (major version >= the server): %w", err)
	}

	nUsers, err := userCount(ctx, psql, opts.DatabaseURL)
	if err != nil {
		return err
	}
	if nUsers > 0 && !opts.Force {
		return fmt.Errorf("restore: target database already has %d user(s); refusing to overwrite — re-run with --force to proceed", nUsers)
	}

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("restore: open gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	dumpFile, err := os.CreateTemp("", "slatedesk-restore-*.sql")
	if err != nil {
		return fmt.Errorf("restore: create temp dump: %w", err)
	}
	defer os.Remove(dumpFile.Name())
	defer dumpFile.Close()

	sawManifest, sawDump := false, false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("restore: read archive: %w", err)
		}
		switch {
		case hdr.Name == manifestName:
			if err := checkManifest(tr); err != nil {
				return err
			}
			sawManifest = true
		case hdr.Name == dumpName:
			if _, err := io.Copy(dumpFile, tr); err != nil {
				return fmt.Errorf("restore: extract dump: %w", err)
			}
			sawDump = true
		case strings.HasPrefix(hdr.Name, attachPrefix):
			if err := restoreAttachment(opts.attachmentsDir(), hdr, tr); err != nil {
				return err
			}
		default:
			// Unknown member — ignore for forward compatibility.
		}
	}
	if !sawManifest {
		return fmt.Errorf("restore: archive has no %s — not a SlateDesk backup", manifestName)
	}
	if !sawDump {
		return fmt.Errorf("restore: archive has no %s", dumpName)
	}
	if err := dumpFile.Close(); err != nil {
		return fmt.Errorf("restore: flush dump: %w", err)
	}

	// Reset to a clean schema so the dump applies deterministically, then
	// apply it with ON_ERROR_STOP so any failure aborts loudly.
	if err := runPsql(ctx, psql, opts.DatabaseURL, nil,
		"-c", "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;"); err != nil {
		return fmt.Errorf("restore: reset schema: %w", err)
	}
	if err := runPsql(ctx, psql, opts.DatabaseURL, nil, "-f", dumpFile.Name()); err != nil {
		return fmt.Errorf("restore: apply dump: %w", err)
	}
	return nil
}

// restoreAttachment writes one attachment member under dir, guarding against
// path traversal (a member must stay inside the attachments directory).
func restoreAttachment(dir string, hdr *tar.Header, r io.Reader) error {
	rel := strings.TrimPrefix(hdr.Name, attachPrefix)
	if rel == "" {
		return nil
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || filepath.IsAbs(clean) {
		return fmt.Errorf("restore: refusing unsafe attachment path %q", hdr.Name)
	}
	dest := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("restore: create attachment dir: %w", err)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("restore: create attachment %s: %w", dest, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return fmt.Errorf("restore: write attachment %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("restore: close attachment %s: %w", dest, err)
	}
	return nil
}

// checkManifest validates the archive is a SlateDesk backup of a format this
// binary understands.
func checkManifest(r io.Reader) error {
	sc := bufio.NewScanner(r)
	first := true
	var version int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			if line != manifestID {
				return fmt.Errorf("restore: not a SlateDesk backup (manifest reads %q)", line)
			}
			first = false
			continue
		}
		if v, ok := strings.CutPrefix(line, "format:"); ok {
			version, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("restore: read manifest: %w", err)
	}
	if version > FormatVersion {
		return fmt.Errorf("restore: backup format %d is newer than this binary supports (%d) — upgrade slatedesk", version, FormatVersion)
	}
	return nil
}

// userCount returns the number of rows in public.users, or 0 if the table
// does not exist yet (a fresh, unmigrated database). It shells out to psql
// so restore needs no database driver of its own.
//
// The existence check is a separate query on purpose: a single
// CASE...(SELECT count(*) FROM public.users) still names the table, which
// Postgres resolves at parse time — it would error on a fresh database
// before the CASE could short-circuit.
func userCount(ctx context.Context, psql, url string) (int, error) {
	exists, err := psqlScalar(ctx, psql, url, "SELECT to_regclass('public.users') IS NOT NULL")
	if err != nil {
		return 0, fmt.Errorf("restore: probe target database: %w", err)
	}
	if exists != "t" {
		return 0, nil
	}
	out, err := psqlScalar(ctx, psql, url, "SELECT count(*) FROM public.users")
	if err != nil {
		return 0, fmt.Errorf("restore: count users: %w", err)
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("restore: parse user count %q: %w", out, err)
	}
	return n, nil
}

// psqlScalar runs a single-value query and returns the trimmed output.
func psqlScalar(ctx context.Context, psql, url, query string) (string, error) {
	var out, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, psql, "--dbname="+url,
		"--no-psqlrc", "--quiet", "-tA", "-c", query)
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// runPsql runs psql with ON_ERROR_STOP so a mid-script failure is fatal.
func runPsql(ctx context.Context, psql, url string, stdin io.Reader, args ...string) error {
	base := []string{"--dbname=" + url, "--no-psqlrc", "--quiet", "--set=ON_ERROR_STOP=1"}
	var errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, psql, append(base, args...)...)
	cmd.Stdin = stdin
	cmd.Stderr = &errBuf
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
	}
	return nil
}
