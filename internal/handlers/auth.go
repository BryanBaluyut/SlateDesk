package handlers

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
)

// dummyHash is a valid argon2id hash of an unguessable throwaway value.
// Login verifies against it when the account does not exist (or has no
// password), so a miss costs the same as a mismatch and response timing
// does not reveal which emails have accounts.
var dummyHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword("slatedesk-dummy-timing-equalizer")
	if err != nil {
		// rand.Read failing is unrecoverable; surface at first login
		// rather than panic at import time.
		return ""
	}
	return h
})

// Login implements POST /auth/login.
func (h *Handlers) Login(w http.ResponseWriter, r *http.Request) {
	var req api.LoginJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	email, err := auth.NormalizeEmail(string(req.Email))
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid email address")
		return
	}
	if req.Password == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "password is required")
		return
	}

	// Aggregate per-IP cap first (short-circuits before a per-email bucket
	// is created), then the per-account brake.
	ip := clientIP(r)
	if !h.ipLimiter.Allow(ip) || !h.limiter.Allow(ip+"|"+email) {
		w.Header().Set("Retry-After", "30")
		problem.Write(w, r, http.StatusTooManyRequests, "Too Many Requests", "too many login attempts; retry later")
		return
	}

	invalidCredentials := func() {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "invalid email or password")
	}

	u, err := h.q.GetUserByEmail(r.Context(), email)
	if err != nil {
		if isNoRows(err) {
			// Burn the same time as a real verification (user enumeration).
			_ = auth.VerifyPassword(req.Password, dummyHash())
			invalidCredentials()
			return
		}
		serverError(w, r, "load user for login", err)
		return
	}
	if !u.Active || !u.PasswordHash.Valid || u.PasswordHash.String == "" {
		_ = auth.VerifyPassword(req.Password, dummyHash())
		invalidCredentials()
		return
	}
	if err := auth.VerifyPassword(req.Password, u.PasswordHash.String); err != nil {
		invalidCredentials()
		return
	}

	token, err := auth.SignSession(h.secret, auth.Session{
		UserID:       u.ID,
		TokenVersion: u.TokenVersion,
		ExpiresAt:    time.Now().Add(h.sessionTTL).Unix(),
	})
	if err != nil {
		serverError(w, r, "sign session", err)
		return
	}
	auth.SetSessionCookie(w, r, token, h.sessionTTL, h.cookieSecure)
	w.WriteHeader(http.StatusNoContent)
}

// Logout implements POST /auth/logout. Idempotent; succeeds for
// unauthenticated callers too (spec: security []). The clearing Set-Cookie
// is only sent when the request actually carried the session cookie:
// SameSite=Lax withholds it from cross-site POSTs, so a hostile page
// cannot force-log-out a victim (CSRF) — its cookieless request is a no-op.
func (h *Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(auth.SessionCookieName); err == nil {
		auth.ClearSessionCookie(w, r, h.cookieSecure)
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetCurrentUser implements GET /auth/me. RequireUser has already
// authenticated the session.
func (h *Handlers) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	u, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	writeJSON(w, r, http.StatusOK, toAPIUser(u))
}

// clientIP extracts the peer IP for rate-limit keying. M1 deliberately uses
// the direct peer (not X-Forwarded-For, which any client can forge); behind
// a reverse proxy all logins share the proxy's IP but remain keyed by email.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
