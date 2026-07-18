package email

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BryanBaluyut/slatedesk/internal/db"
	"github.com/BryanBaluyut/slatedesk/internal/migrate"
	"github.com/BryanBaluyut/slatedesk/internal/secrets"
)

// Same throwaway-database pattern as internal/handlers/main_test.go.
// migrate.Run also applies River's migrations, so the queue tables exist.

const defaultDatabaseURL = "postgres://slatedesk:slatedesk_dev@127.0.0.1:5432/slatedesk?sslmode=disable"

var (
	testPool *pgxpool.Pool
	// testBox encrypts mailbox credentials in tests (random per-run root
	// secret; the DB is throwaway anyway).
	testBox *secrets.Box
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		baseURL = defaultDatabaseURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		log.Printf("email test: generate db suffix: %v", err)
		return 1
	}
	testDBName := fmt.Sprintf("slatedesk_test_%d_%s", os.Getpid(), hex.EncodeToString(suffix[:]))

	admin, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		log.Printf("email test: connect to %s: %v (is the dev Postgres running?)", baseURL, err)
		return 1
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{testDBName}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		log.Printf("email test: create test database: %v", err)
		return 1
	}
	defer func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		if _, err := admin.Exec(dropCtx, "DROP DATABASE "+pgx.Identifier{testDBName}.Sanitize()+" WITH (FORCE)"); err != nil {
			log.Printf("email test: drop test database %s: %v", testDBName, err)
		}
		_ = admin.Close(dropCtx)
	}()

	poolCfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		log.Printf("email test: parse DATABASE_URL: %v", err)
		return 1
	}
	poolCfg.ConnConfig.Database = testDBName
	// Mirror production pool setup (tsvector codec registration).
	poolCfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		db.RegisterTypes(conn)
		return nil
	}
	testPool, err = pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Printf("email test: connect to test database: %v", err)
		return 1
	}
	defer testPool.Close()

	if err := migrate.Run(ctx, testPool); err != nil {
		log.Printf("email test: migrate: %v", err)
		return 1
	}

	rootSecret := make([]byte, 32)
	if _, err := rand.Read(rootSecret); err != nil {
		log.Printf("email test: generate root secret: %v", err)
		return 1
	}
	testBox, err = secrets.NewBox(rootSecret, secrets.PurposeMailboxCredentials)
	if err != nil {
		log.Printf("email test: secrets box: %v", err)
		return 1
	}

	return m.Run()
}
