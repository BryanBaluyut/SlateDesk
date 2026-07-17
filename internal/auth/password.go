// Package auth provides the local-auth primitives for M1: argon2id password
// hashing and signed session tokens carried in an HttpOnly cookie.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. Stored in the PHC hash string, so they can be tuned
// later without invalidating existing hashes.
const (
	argonTime    = 3         // iterations
	argonMemory  = 64 * 1024 // KiB = 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrPasswordMismatch is returned by VerifyPassword when the password does
// not match the stored hash.
var ErrPasswordMismatch = errors.New("auth: password does not match")

// maxConcurrentArgon2 caps concurrent argon2id computations. Each one pins
// argonMemory (64 MiB) for its duration, so unbounded parallelism — e.g. a
// flood of login attempts — is a memory-exhaustion vector. 4 × 64 MiB is a
// 256 MiB worst case, and the resulting throughput (tens of hashes/sec)
// far exceeds any legitimate login rate; excess callers just queue briefly.
const maxConcurrentArgon2 = 4

var argon2Sem = make(chan struct{}, maxConcurrentArgon2)

// argon2Key wraps argon2.IDKey with the concurrency cap above.
func argon2Key(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
	argon2Sem <- struct{}{}
	defer func() { <-argon2Sem }()
	return argon2.IDKey(password, salt, time, memory, threads, keyLen)
}

// HashPassword hashes a password with argon2id and returns a PHC-format
// string: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	key := argon2Key([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks password against a PHC argon2id hash produced by
// HashPassword (or any compatible implementation). It returns nil on match,
// ErrPasswordMismatch on mismatch, and another error for malformed hashes.
func VerifyPassword(password, phcHash string) error {
	parts := strings.Split(phcHash, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return errors.New("auth: malformed argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return fmt.Errorf("auth: parse hash version: %w", err)
	}
	if version != argon2.Version {
		return fmt.Errorf("auth: unsupported argon2 version %d", version)
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return fmt.Errorf("auth: parse hash params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("auth: decode hash salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("auth: decode hash key: %w", err)
	}

	got := argon2Key([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}
