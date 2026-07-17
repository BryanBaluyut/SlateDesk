// Command slatedesk is the SlateDesk server binary.
//
// Subcommands:
//
//	serve         start the HTTP server (default; applies migrations first)
//	migrate       apply pending database migrations and exit
//	admin create  create or update an admin user
//
// Configuration comes from the environment: DATABASE_URL (required),
// SLATEDESK_ADDR (default :8000), and optionally SLATEDESK_ADMIN_EMAIL /
// SLATEDESK_ADMIN_PASSWORD for headless admin bootstrap on serve.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/config"
	"github.com/BryanBaluyut/slatedesk/internal/db"
	"github.com/BryanBaluyut/slatedesk/internal/httpserver"
	"github.com/BryanBaluyut/slatedesk/internal/migrate"
	"github.com/BryanBaluyut/slatedesk/internal/settings"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "migrate":
		err = cmdMigrate(args)
	case "admin":
		err = cmdAdmin(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "slatedesk: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	if err != nil {
		slog.Error("fatal", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `Usage: slatedesk [command]

Commands:
  serve         Start the HTTP server (default). Applies migrations first.
  migrate       Apply pending database migrations and exit.
  admin create  Create or update an admin user.
  help          Show this help.

Environment:
  DATABASE_URL              Postgres connection string (required)
  SLATEDESK_ADDR            HTTP listen address (default :8000)
  SLATEDESK_ADMIN_EMAIL     Bootstrap admin email (serve / admin create)
  SLATEDESK_ADMIN_PASSWORD  Bootstrap admin password (serve / admin create)
  SLATEDESK_COOKIE_SECURE   Session cookie Secure attribute: auto (default),
                            always, or never
`)
}

// signalContext returns a context canceled on SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addrFlag := fs.String("addr", "", "listen address (overrides SLATEDESK_ADDR)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *addrFlag != "" {
		cfg.Addr = *addrFlag
	}

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := migrate.Run(ctx, pool); err != nil {
		return err
	}

	secret, err := settings.New(pool).EnsureInstanceSecret(ctx)
	if err != nil {
		return err
	}
	slog.Info("instance secret ready")

	// Headless/IaC bootstrap (architecture doc §5.5): create the initial
	// admin from the environment, but only when no active admin exists yet
	// — a running instance's admins are never overwritten by env vars.
	// Idempotent across restarts. Use `slatedesk admin create` to reset an
	// existing admin explicitly.
	if cfg.AdminEmail != "" && cfg.AdminPassword != "" {
		adminExists, err := store.New(pool).AdminExists(ctx)
		if err != nil {
			return fmt.Errorf("bootstrap admin from env: check for existing admin: %w", err)
		}
		if adminExists {
			slog.Info("admin bootstrap skipped: an active admin already exists")
		} else if err := ensureAdmin(ctx, pool, cfg.AdminEmail, "", cfg.AdminPassword); err != nil {
			return fmt.Errorf("bootstrap admin from env: %w", err)
		}
	}

	return httpserver.New(cfg.Addr, pool, secret, cfg.CookieSecure).Run(ctx)
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := migrate.Run(ctx, pool); err != nil {
		return err
	}
	slog.Info("migrations up to date")
	return nil
}

func cmdAdmin(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return fmt.Errorf("usage: slatedesk admin create -email <email> [-name <name>] [-password <password>]")
	}

	fs := flag.NewFlagSet("admin create", flag.ExitOnError)
	email := fs.String("email", os.Getenv("SLATEDESK_ADMIN_EMAIL"), "admin email (or SLATEDESK_ADMIN_EMAIL)")
	name := fs.String("name", "", "admin display name")
	password := fs.String("password", os.Getenv("SLATEDESK_ADMIN_PASSWORD"), "admin password (or SLATEDESK_ADMIN_PASSWORD)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return fmt.Errorf("admin create: -email and -password are required (or SLATEDESK_ADMIN_EMAIL / SLATEDESK_ADMIN_PASSWORD)")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrations first so admin create works against a fresh database.
	if err := migrate.Run(ctx, pool); err != nil {
		return err
	}

	if err := ensureAdmin(ctx, pool, *email, *name, *password); err != nil {
		return err
	}
	return nil
}

// ensureAdmin creates the admin user, or — if a user with that email already
// exists — promotes them to admin, resets their password, and reactivates
// them. Idempotent by design. The password is never logged.
func ensureAdmin(ctx context.Context, pool *pgxpool.Pool, email, name, password string) error {
	normalized, err := auth.NormalizeEmail(email)
	if err != nil {
		return fmt.Errorf("invalid admin email: %w", err)
	}
	if err := auth.ValidatePassword(password); err != nil {
		return fmt.Errorf("invalid admin password: %w", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	u, err := store.New(pool).UpsertAdmin(ctx, store.UpsertAdminParams{
		Email:        normalized,
		Name:         name,
		PasswordHash: pgtype.Text{String: hash, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("upsert admin %q: %w", normalized, err)
	}
	slog.Info("admin user ready", "email", u.Email, "id", u.ID)
	return nil
}
