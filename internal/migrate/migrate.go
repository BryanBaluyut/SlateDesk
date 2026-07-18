// Package migrate applies embedded SQL migrations.
//
// Migrations live in sql/ as NNNN_name.sql files, embedded into the binary.
// Applied versions are recorded in the schema_migrations table. The whole
// run is serialized across replicas with a session-level Postgres advisory
// lock, so N app nodes can start simultaneously and exactly one applies each
// pending migration (architecture doc §5/§6: migrations self-apply on start).
// River's queue-table migrations run inside the same locked critical section
// (see jobs.Migrate), so one migrate step produces the complete schema.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BryanBaluyut/slatedesk/internal/jobs"
)

//go:embed sql/*.sql
var migrationsFS embed.FS

// lockKey returns the advisory lock key used to serialize migration runs.
// Derived from a fixed string so it is stable across builds.
func lockKey() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("slatedesk:migrations"))
	return int64(h.Sum64())
}

// Run applies all pending migrations. It is safe to call from multiple
// processes concurrently; the advisory lock ensures only one applies at a
// time and the rest observe the finished state.
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	names, err := migrationNames()
	if err != nil {
		return err
	}

	// A dedicated connection holds the session-level advisory lock; it is
	// released explicitly below and, as a backstop, when the session ends.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	key := lockKey()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	defer func() {
		// Best-effort unlock; the lock also dies with the session.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("migrate: ensure schema_migrations table: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("migrate: read applied versions: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("migrate: scan applied versions: %w", err)
	}
	for _, v := range versions {
		applied[v] = true
	}

	for _, name := range names {
		if applied[name] {
			continue
		}
		if err := applyOne(ctx, conn.Conn(), name); err != nil {
			return err
		}
		slog.Info("applied migration", "version", name)
	}

	// River's own migrations (river_job etc.) are part of our migrate step:
	// still inside the advisory-lock critical section, so exactly one
	// replica applies them. They use pool connections, not the lock-holding
	// conn — fine, the lock serializes callers of Run, not connections.
	if err := jobs.Migrate(ctx, pool); err != nil {
		return err
	}
	return nil
}

// applyOne runs a single migration file and records it, in one transaction.
func applyOne(ctx context.Context, conn *pgx.Conn, name string) error {
	sqlBytes, err := migrationsFS.ReadFile("sql/" + name)
	if err != nil {
		return fmt.Errorf("migrate: read %s: %w", name, err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin tx for %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Simple protocol so a file may contain multiple SQL statements.
	if _, err := tx.Exec(ctx, string(sqlBytes), pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("migrate: apply %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
		return fmt.Errorf("migrate: record %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit %s: %w", name, err)
	}
	return nil
}

// migrationNames lists embedded migration files in apply order.
func migrationNames() ([]string, error) {
	entries, err := migrationsFS.ReadDir("sql")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded sql dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
