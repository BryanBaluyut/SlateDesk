// Package db initializes the pgx connection pool.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tsvectorOID is the fixed catalog OID of the built-in tsvector type.
const tsvectorOID = 3614

// RegisterTypes teaches a pgx connection about Postgres types it has no
// built-in codec for. tsvector (the generated search_tsv columns) is
// registered with a text codec so `SELECT *` / `RETURNING *` on tickets
// and articles scans cleanly into the sqlc models (we never parse the
// value — search runs entirely in SQL). Every pool must install this via
// AfterConnect (Connect below does; test harnesses mirror it).
func RegisterTypes(conn *pgx.Conn) {
	conn.TypeMap().RegisterType(&pgtype.Type{Name: "tsvector", OID: tsvectorOID, Codec: pgtype.TextCodec{}})
}

// connectTimeout bounds how long Connect keeps retrying the initial ping.
// Postgres may still be starting (e.g. docker compose bringing both
// containers up at once), so we retry with backoff instead of failing fast.
const connectTimeout = 60 * time.Second

// Connect creates a pgxpool and waits until the database answers a ping,
// retrying with capped exponential backoff. It returns an error if the
// database is still unreachable after connectTimeout, or if ctx is canceled.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}
	poolCfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		RegisterTypes(conn)
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}

	deadline := time.Now().Add(connectTimeout)
	backoff := 250 * time.Millisecond
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if ctx.Err() != nil {
			pool.Close()
			return nil, fmt.Errorf("db: canceled while waiting for database: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("db: database not reachable after %s: %w", connectTimeout, err)
		}

		slog.Warn("database not ready, retrying", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("db: canceled while waiting for database: %w", ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
}
