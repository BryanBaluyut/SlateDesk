// Package secrets encrypts small secrets (mailbox credentials, OAuth
// tokens) at rest with AES-256-GCM.
//
// Keys are never stored: each purpose gets its own 32-byte key derived from
// the instance root secret (settings.EnsureInstanceSecret) via HKDF-SHA256,
// with the purpose string as the HKDF info parameter. Different purposes
// therefore yield unrelated keys, and a ciphertext for one purpose can never
// be opened under another (architecture doc §4: mailbox credentials are
// AES-GCM-encrypted with a key derived from the root secret).
//
// Ciphertext format is versioned so the scheme can evolve without ambiguity:
//
//	v1:base64std(nonce || aes-256-gcm ciphertext+tag)
//
// The purpose string is additionally bound into the GCM additional data, so
// even a hypothetical key collision across purposes would fail to decrypt.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Purpose names. Each is an HKDF info string; changing one is a breaking
// key rotation for everything encrypted under it, so treat them as frozen.
const (
	// PurposeMailboxCredentials protects the mailboxes.credentials_enc
	// JSON blob (passwords, OAuth client secrets / refresh tokens).
	PurposeMailboxCredentials = "mailbox-credentials"
)

// v1Prefix tags the current ciphertext format.
const v1Prefix = "v1:"

// keyLen is the AES-256 key size.
const keyLen = 32

// ErrInvalidCiphertext is returned when a ciphertext is malformed, has an
// unknown version, was tampered with, or was encrypted under a different
// key (GCM authentication cannot distinguish the last two).
var ErrInvalidCiphertext = errors.New("secrets: invalid ciphertext")

// Box seals and opens secrets for one purpose. It is safe for concurrent
// use. Construct via NewBox.
type Box struct {
	purpose string
	aead    cipher.AEAD
}

// NewBox derives the purpose key from the 32-byte instance root secret and
// returns a Box for that purpose.
func NewBox(instanceSecret []byte, purpose string) (*Box, error) {
	if len(instanceSecret) != keyLen {
		return nil, fmt.Errorf("secrets: instance secret must be %d bytes, got %d", keyLen, len(instanceSecret))
	}
	if purpose == "" {
		return nil, errors.New("secrets: purpose must not be empty")
	}

	// HKDF-SHA256(secret, salt=nil, info=purpose) -> 32-byte AES key.
	// A nil salt is fine per RFC 5869: the instance secret is already a
	// uniformly random key, and the info string separates purposes.
	key, err := hkdf.Key(sha256.New, instanceSecret, nil, purpose, keyLen)
	if err != nil {
		return nil, fmt.Errorf("secrets: derive key for %q: %w", purpose, err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: init AES for %q: %w", purpose, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: init GCM for %q: %w", purpose, err)
	}
	return &Box{purpose: purpose, aead: aead}, nil
}

// Encrypt seals plaintext and returns the versioned, base64-encoded
// ciphertext (safe to store in a text column).
func (b *Box) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: generate nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, plaintext, []byte(b.purpose))
	return v1Prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a ciphertext produced by Encrypt for the same purpose.
// Any tampering, truncation, or key/purpose mismatch yields
// ErrInvalidCiphertext.
func (b *Box) Decrypt(ciphertext string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(ciphertext, v1Prefix)
	if !ok {
		return nil, fmt.Errorf("%w: unknown or missing version prefix", ErrInvalidCiphertext)
	}
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: bad base64: %v", ErrInvalidCiphertext, err)
	}
	if len(sealed) < b.aead.NonceSize() {
		return nil, fmt.Errorf("%w: too short", ErrInvalidCiphertext)
	}
	nonce, ct := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ct, []byte(b.purpose))
	if err != nil {
		// GCM auth failure: tampered, or wrong key/purpose. Deliberately
		// not distinguishable; do not leak the underlying error text.
		return nil, fmt.Errorf("%w: authentication failed", ErrInvalidCiphertext)
	}
	return plaintext, nil
}
