package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionCookieName is the name of the browser session cookie. It must match
// the cookieAuth security scheme in api/openapi.yaml.
const SessionCookieName = "sd_session"

// DefaultSessionTTL is how long a freshly issued session token lives.
const DefaultSessionTTL = 30 * 24 * time.Hour

// ErrInvalidToken is returned when a session token fails signature or
// structural validation, or has expired. It deliberately carries no detail.
var ErrInvalidToken = errors.New("auth: invalid session token")

// Session is the payload carried by the signed session cookie.
// TokenVersion is compared against users.token_version on every request:
// bumping the column invalidates all outstanding tokens for that user
// (logout-everywhere for free — architecture doc §4).
type Session struct {
	UserID       uuid.UUID `json:"uid"`
	TokenVersion int32     `json:"tv"`
	ExpiresAt    int64     `json:"exp"` // unix seconds
}

// SignSession serializes and signs a session with HMAC-SHA256 under the
// instance secret. Format: base64url(payload) + "." + base64url(mac).
func SignSession(secret []byte, s Session) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("auth: encode session: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + sign(secret, encoded), nil
}

// VerifySession validates the signature and expiry of a token produced by
// SignSession and returns the embedded session.
func VerifySession(secret []byte, token string) (Session, error) {
	encoded, mac, ok := strings.Cut(token, ".")
	if !ok {
		return Session{}, ErrInvalidToken
	}
	if !hmac.Equal([]byte(sign(secret, encoded)), []byte(mac)) {
		return Session{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Session{}, ErrInvalidToken
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}, ErrInvalidToken
	}
	if time.Now().Unix() >= s.ExpiresAt {
		return Session{}, ErrInvalidToken
	}
	return s, nil
}

// CookieSecureMode controls when session cookies carry the Secure
// attribute (SLATEDESK_COOKIE_SECURE).
type CookieSecureMode string

const (
	// CookieSecureAuto sets Secure when the request arrived over TLS,
	// directly or per a proxy's X-Forwarded-Proto. The header is
	// client-suppliable when no proxy normalizes it, but spoofing it only
	// hardens the spoofer's own cookie (Secure), which is harmless. The
	// gap is a TLS-terminating proxy that does not send X-Forwarded-Proto:
	// auto cannot see the TLS hop, so such deployments must set
	// SLATEDESK_COOKIE_SECURE=always.
	CookieSecureAuto CookieSecureMode = "auto"
	// CookieSecureAlways marks cookies Secure unconditionally.
	CookieSecureAlways CookieSecureMode = "always"
	// CookieSecureNever omits Secure (plain-HTTP-only deployments).
	CookieSecureNever CookieSecureMode = "never"
)

// secure resolves the Secure attribute for a request under this mode.
func (m CookieSecureMode) secure(r *http.Request) bool {
	switch m {
	case CookieSecureAlways:
		return true
	case CookieSecureNever:
		return false
	default: // auto
		return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
}

// SetSessionCookie writes the signed session token as an HttpOnly,
// SameSite=Lax cookie. Secure is decided by mode (see CookieSecureMode).
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration, mode CookieSecureMode) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   mode.secure(r),
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request, mode CookieSecureMode) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   mode.secure(r),
	})
}

func sign(secret []byte, msg string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
