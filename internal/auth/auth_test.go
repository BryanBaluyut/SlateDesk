package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPasswordHashRoundtrip(t *testing.T) {
	hash, err := HashPassword("s3cret-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	if err := VerifyPassword("s3cret-password", hash); err != nil {
		t.Fatalf("VerifyPassword with correct password: %v", err)
	}
	if err := VerifyPassword("wrong-password", hash); !errors.Is(err, ErrPasswordMismatch) {
		t.Fatalf("VerifyPassword with wrong password: got %v, want ErrPasswordMismatch", err)
	}
}

func TestVerifyPasswordMalformedHash(t *testing.T) {
	for _, h := range []string{"", "plainhash", "$argon2i$v=19$m=1,t=1,p=1$a$b"} {
		if err := VerifyPassword("x", h); err == nil {
			t.Fatalf("VerifyPassword(%q): expected error, got nil", h)
		}
	}
}

func TestSessionSignVerifyRoundtrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	want := Session{
		UserID:       uuid.New(),
		TokenVersion: 3,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	token, err := SignSession(secret, want)
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	got, err := VerifySession(secret, token)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if got != want {
		t.Fatalf("session roundtrip: got %+v, want %+v", got, want)
	}
}

func TestVerifySessionRejects(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s := Session{UserID: uuid.New(), TokenVersion: 1, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	token, err := SignSession(secret, s)
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}

	if _, err := VerifySession([]byte("another-secret-another-secret-32"), token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong secret: got %v, want ErrInvalidToken", err)
	}
	if _, err := VerifySession(secret, token+"x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered token: got %v, want ErrInvalidToken", err)
	}
	if _, err := VerifySession(secret, "no-dot-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("malformed token: got %v, want ErrInvalidToken", err)
	}

	expired := Session{UserID: s.UserID, TokenVersion: 1, ExpiresAt: time.Now().Add(-time.Minute).Unix()}
	expiredToken, err := SignSession(secret, expired)
	if err != nil {
		t.Fatalf("SignSession expired: %v", err)
	}
	if _, err := VerifySession(secret, expiredToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token: got %v, want ErrInvalidToken", err)
	}
}
