package apikey

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestGenerateRoundtrip(t *testing.T) {
	g, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Plaintext shape: prefix + fixed-width base62 body.
	if !strings.HasPrefix(g.Plaintext, Prefix) {
		t.Errorf("plaintext %q missing prefix %q", g.Plaintext, Prefix)
	}
	body := strings.TrimPrefix(g.Plaintext, Prefix)
	if len(body) != encodedLen {
		t.Errorf("body length = %d, want %d", len(body), encodedLen)
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(base62Alphabet, body[i]) < 0 {
			t.Errorf("body has non-base62 byte %q", body[i])
		}
	}
	if !HasValidFormat(g.Plaintext) {
		t.Errorf("HasValidFormat(%q) = false, want true", g.Plaintext)
	}

	// Display prefix is a safe, strict prefix of the secret (never the whole).
	if !strings.HasPrefix(g.Plaintext, g.Prefix) {
		t.Errorf("display prefix %q is not a prefix of plaintext %q", g.Prefix, g.Plaintext)
	}
	if len(g.Prefix) >= len(g.Plaintext) {
		t.Errorf("display prefix %q leaks the full secret", g.Prefix)
	}
	if want := Prefix + body[:displayChars]; g.Prefix != want {
		t.Errorf("display prefix = %q, want %q", g.Prefix, want)
	}

	// Hash is the SHA-256 hex of the plaintext and matches Hash().
	if g.Hash != Hash(g.Plaintext) {
		t.Errorf("stored hash %q != Hash(plaintext) %q", g.Hash, Hash(g.Plaintext))
	}
	if len(g.Hash) != HashLen {
		t.Errorf("hash length = %d, want %d", len(g.Hash), HashLen)
	}
	if _, err := hex.DecodeString(g.Hash); err != nil {
		t.Errorf("hash is not hex: %v", err)
	}
	// The stored hash must not be (or contain) the plaintext.
	if strings.Contains(g.Hash, body) {
		t.Errorf("hash appears to embed the plaintext secret")
	}

	// Roundtrip: the plaintext verifies against its stored hash.
	if !Verify(g.Plaintext, g.Hash) {
		t.Errorf("Verify(plaintext, hash) = false, want true")
	}
}

func TestGenerateUnique(t *testing.T) {
	const n = 200
	seenPlain := make(map[string]bool, n)
	seenHash := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		g, err := Generate()
		if err != nil {
			t.Fatalf("Generate #%d: %v", i, err)
		}
		if seenPlain[g.Plaintext] {
			t.Fatalf("duplicate plaintext at #%d", i)
		}
		if seenHash[g.Hash] {
			t.Fatalf("duplicate hash at #%d", i)
		}
		seenPlain[g.Plaintext] = true
		seenHash[g.Hash] = true
	}
}

func TestVerifyWrongKey(t *testing.T) {
	g, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	other, err := Generate()
	if err != nil {
		t.Fatalf("Generate other: %v", err)
	}

	cases := []struct {
		name      string
		candidate string
		hash      string
		want      bool
	}{
		{"correct", g.Plaintext, g.Hash, true},
		{"different key", other.Plaintext, g.Hash, false},
		{"tampered last char", flipLast(g.Plaintext), g.Hash, false},
		{"empty candidate", "", g.Hash, false},
		{"prefix only", Prefix, g.Hash, false},
		{"hash as candidate", g.Hash, g.Hash, false},
		{"empty stored hash", g.Plaintext, "", false},
		{"truncated stored hash", g.Plaintext, g.Hash[:len(g.Hash)-1], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Verify(tc.candidate, tc.hash); got != tc.want {
				t.Errorf("Verify(%q, %q) = %v, want %v", tc.candidate, tc.hash, got, tc.want)
			}
		})
	}
}

func TestHashDeterministic(t *testing.T) {
	const in = "sd_live_whatever"
	if Hash(in) != Hash(in) {
		t.Errorf("Hash is not deterministic")
	}
	if Hash(in) == Hash(in+"x") {
		t.Errorf("distinct inputs collided")
	}
}

func TestHasValidFormat(t *testing.T) {
	good, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"generated", good.Plaintext, true},
		{"empty", "", false},
		{"prefix only", Prefix, false},
		{"no prefix", strings.TrimPrefix(good.Plaintext, Prefix), false},
		{"wrong prefix", "sd_test_" + strings.TrimPrefix(good.Plaintext, Prefix), false},
		{"too short", Prefix + "abc", false},
		{"too long", good.Plaintext + "a", false},
		{"non-base62 char", Prefix + strings.Repeat("_", encodedLen), false},
		{"trailing space", good.Plaintext + " ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasValidFormat(tc.in); got != tc.want {
				t.Errorf("HasValidFormat(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// flipLast returns s with its final byte changed to a different base62 digit,
// preserving valid format while guaranteeing a distinct plaintext.
func flipLast(s string) string {
	if s == "" {
		return "0"
	}
	last := s[len(s)-1]
	repl := byte('0')
	if last == '0' {
		repl = '1'
	}
	return s[:len(s)-1] + string(repl)
}
