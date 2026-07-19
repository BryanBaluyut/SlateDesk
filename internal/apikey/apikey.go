// Package apikey mints and verifies scoped API keys for machine access to
// /api/v1 (architecture doc §4: "scoped API keys, sd_live_-prefixed,
// SHA-256-hashed, Bearer-only").
//
// A key is a public prefix plus 32 cryptographically random bytes rendered in
// base62. The full plaintext (sd_live_…) is returned to the operator exactly
// once, at creation; only its SHA-256 hex digest is persisted (api_keys.key_hash,
// UNIQUE). Authentication hashes the presented key and looks the digest up by
// that unique column, so the secret is only ever compared as a hash inside
// Postgres' index — there is no plaintext at rest and no application-level
// string comparison of the secret.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// Prefix marks every SlateDesk API key. The "live" band leaves room for a
// future "test" band (sd_test_) without colliding on the hash space.
const Prefix = "sd_live_"

const (
	// secretBytes is the random-entropy length behind each key: 256 bits.
	secretBytes = 32

	// encodedLen is the fixed base62 width of secretBytes bytes.
	// ceil(256 * ln2 / ln62) = 43, and 62^43 > 2^256 >= 62^42, so 43 digits
	// represent any 256-bit value and no fewer always suffice.
	encodedLen = 43

	// displayChars is how many leading base62 characters ride along in the
	// stored, non-secret key_prefix for at-a-glance identification.
	displayChars = 6

	// HashLen is the hex length of a stored key_hash (SHA-256).
	HashLen = sha256.Size * 2
)

// base62Alphabet orders digits < uppercase < lowercase; index 0 ('0') is the
// zero digit used to left-pad short encodings to encodedLen.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Generated is the one-time output of Generate. Only Prefix and Hash are safe
// to persist; Plaintext must be shown to the operator and then dropped.
type Generated struct {
	// Plaintext is the full key (Prefix + base62 body) — returned once, never
	// stored, never logged.
	Plaintext string
	// Prefix is the non-secret display fragment (e.g. "sd_live_ab12cd") stored
	// in api_keys.key_prefix so the admin UI can identify a key.
	Prefix string
	// Hash is the SHA-256 hex digest of Plaintext, the only representation
	// persisted (api_keys.key_hash).
	Hash string
}

// Generate mints a fresh key from 32 bytes of crypto/rand entropy.
func Generate() (Generated, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Generated{}, fmt.Errorf("apikey: read random bytes: %w", err)
	}
	body := encodeBase62(raw)
	plaintext := Prefix + body
	return Generated{
		Plaintext: plaintext,
		Prefix:    Prefix + body[:displayChars],
		Hash:      Hash(plaintext),
	}, nil
}

// Hash returns the SHA-256 hex digest of a plaintext key. This is what the
// authentication path computes on the presented key before looking it up by
// the unique key_hash column, and what CreateAPIKey stores.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// HasValidFormat reports whether key is shaped like a SlateDesk key: the
// prefix followed by exactly encodedLen base62 characters. A cheap gate the
// Bearer middleware can apply before a DB round trip; it says nothing about
// whether the key exists.
func HasValidFormat(key string) bool {
	body, ok := strings.CutPrefix(key, Prefix)
	if !ok || len(body) != encodedLen {
		return false
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(base62Alphabet, body[i]) < 0 {
			return false
		}
	}
	return true
}

// Verify reports whether candidate is the plaintext key behind storedHash,
// comparing digests in constant time.
//
// The normal authentication path does NOT use Verify: it calls Hash on the
// presented key and resolves it via store.GetAPIKeyForAuth, an exact-match
// lookup on the unique key_hash index. Because the stored value is a SHA-256
// of a 256-bit random secret, a timing side channel on that lookup would
// require an attacker to already hold the full hash preimage, so no
// constant-time compare is needed there. Verify covers the paths that already
// have both a candidate and a known hash in hand (tests, or any future
// in-memory check) and must not leak equality through timing.
func Verify(candidate, storedHash string) bool {
	got := Hash(candidate)
	// ConstantTimeCompare returns 0 on length mismatch without early-exit
	// timing dependence on content; both operands here are fixed-width hex.
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}

// encodeBase62 renders b as a fixed-width (encodedLen) base62 string,
// left-padded with the zero digit. Full 256-bit entropy is preserved; the
// fixed width keeps every key the same length.
func encodeBase62(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(int64(len(base62Alphabet)))
	out := make([]byte, encodedLen)
	for i := range out {
		out[i] = base62Alphabet[0]
	}
	mod := new(big.Int)
	i := encodedLen - 1
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out[i] = base62Alphabet[mod.Int64()]
		i--
	}
	return string(out)
}
