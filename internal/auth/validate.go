package auth

import (
	"fmt"
	"net/mail"
	"strings"
)

// MinPasswordLen is the minimum accepted password length (bytes) for any
// password set through the API or the admin bootstrap.
const MinPasswordLen = 10

// NormalizeEmail lowercases and trims an email address and validates its
// format. It rejects addresses with display names ("A <a@b.c>") — only the
// bare addr-spec form is stored.
func NormalizeEmail(email string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(email))
	if e == "" {
		return "", fmt.Errorf("auth: email is required")
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e {
		return "", fmt.Errorf("auth: invalid email address %q", email)
	}
	return e, nil
}

// ValidatePassword enforces the password policy (length only in M1).
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("auth: password must be at least %d characters", MinPasswordLen)
	}
	return nil
}
