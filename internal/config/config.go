// Package config loads SlateDesk configuration from the environment.
//
// The required env surface is deliberately tiny (architecture doc §5):
// DATABASE_URL is the only mandatory variable; everything else has a
// sensible default or is optional.
package config

import (
	"fmt"
	"os"

	"github.com/BryanBaluyut/slatedesk/internal/auth"
)

// Config holds all runtime configuration for the slatedesk binary.
type Config struct {
	// DatabaseURL is the Postgres connection string. Required.
	DatabaseURL string

	// Addr is the HTTP listen address. SLATEDESK_ADDR, default ":8000".
	Addr string

	// AdminEmail and AdminPassword, when both set, bootstrap (or update)
	// an admin user on serve start. SLATEDESK_ADMIN_EMAIL /
	// SLATEDESK_ADMIN_PASSWORD. Optional; intended for headless/IaC setups.
	AdminEmail    string
	AdminPassword string

	// CookieSecure controls the session cookie's Secure attribute.
	// SLATEDESK_COOKIE_SECURE: "auto" (default: infer from TLS or
	// X-Forwarded-Proto), "always" (use behind a TLS-terminating proxy
	// that does not send X-Forwarded-Proto), or "never".
	CookieSecure auth.CookieSecureMode
}

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		Addr:          os.Getenv("SLATEDESK_ADDR"),
		AdminEmail:    os.Getenv("SLATEDESK_ADMIN_EMAIL"),
		AdminPassword: os.Getenv("SLATEDESK_ADMIN_PASSWORD"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required (e.g. postgres://user:pass@host:5432/slatedesk)")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8000"
	}
	switch mode := auth.CookieSecureMode(os.Getenv("SLATEDESK_COOKIE_SECURE")); mode {
	case "":
		cfg.CookieSecure = auth.CookieSecureAuto
	case auth.CookieSecureAuto, auth.CookieSecureAlways, auth.CookieSecureNever:
		cfg.CookieSecure = mode
	default:
		return Config{}, fmt.Errorf("config: SLATEDESK_COOKIE_SECURE must be auto, always, or never (got %q)", mode)
	}
	return cfg, nil
}
