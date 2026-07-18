package secrets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testSecret(t *testing.T) []byte {
	t.Helper()
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		t.Fatalf("generate test secret: %v", err)
	}
	return s
}

func TestRoundtrip(t *testing.T) {
	box, err := NewBox(testSecret(t), PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}

	for _, plaintext := range [][]byte{
		[]byte(`{"password":"hunter2"}`),
		[]byte(""),
		bytes.Repeat([]byte{0xff}, 4096),
	} {
		ct, err := box.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if !strings.HasPrefix(ct, "v1:") {
			t.Fatalf("ciphertext missing v1 prefix: %q", ct)
		}
		got, err := box.Decrypt(ct)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Fatalf("roundtrip mismatch: got %q want %q", got, plaintext)
		}
	}
}

func TestEncryptIsRandomized(t *testing.T) {
	box, err := NewBox(testSecret(t), PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	a, err := box.Encrypt([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := box.Encrypt([]byte("same plaintext"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if a == b {
		t.Fatal("two encryptions of the same plaintext produced identical ciphertexts (nonce reuse?)")
	}
}

func TestTamperDetected(t *testing.T) {
	box, err := NewBox(testSecret(t), PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	ct, err := box.Encrypt([]byte("attack at dawn"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip one bit in every byte position of the decoded payload; each
	// mutation must fail authentication.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ct, "v1:"))
	if err != nil {
		t.Fatalf("decode ciphertext: %v", err)
	}
	for i := range raw {
		mutated := bytes.Clone(raw)
		mutated[i] ^= 0x01
		_, err := box.Decrypt("v1:" + base64.StdEncoding.EncodeToString(mutated))
		if !errors.Is(err, ErrInvalidCiphertext) {
			t.Fatalf("tampered byte %d: got err %v, want ErrInvalidCiphertext", i, err)
		}
	}

	// Truncation must fail too.
	if _, err := box.Decrypt("v1:" + base64.StdEncoding.EncodeToString(raw[:len(raw)-1])); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("truncated: got err %v, want ErrInvalidCiphertext", err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	box1, err := NewBox(testSecret(t), PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	box2, err := NewBox(testSecret(t), PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	ct, err := box1.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := box2.Decrypt(ct); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("wrong instance secret: got err %v, want ErrInvalidCiphertext", err)
	}
}

func TestWrongPurposeFails(t *testing.T) {
	secret := testSecret(t)
	boxA, err := NewBox(secret, PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	boxB, err := NewBox(secret, "some-other-purpose")
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	ct, err := boxA.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := boxB.Decrypt(ct); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("cross-purpose decrypt: got err %v, want ErrInvalidCiphertext", err)
	}
}

func TestMalformedInputs(t *testing.T) {
	box, err := NewBox(testSecret(t), PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	for _, ct := range []string{
		"",
		"v0:AAAA",
		"v2:AAAA",
		"not-a-ciphertext",
		"v1:!!!not-base64!!!",
		"v1:" + base64.StdEncoding.EncodeToString([]byte("short")), // < nonce size
	} {
		if _, err := box.Decrypt(ct); !errors.Is(err, ErrInvalidCiphertext) {
			t.Errorf("Decrypt(%q): got err %v, want ErrInvalidCiphertext", ct, err)
		}
	}
}

func TestNewBoxValidation(t *testing.T) {
	if _, err := NewBox([]byte("too short"), PurposeMailboxCredentials); err == nil {
		t.Error("NewBox with short secret: want error, got nil")
	}
	if _, err := NewBox(testSecret(t), ""); err == nil {
		t.Error("NewBox with empty purpose: want error, got nil")
	}
}
