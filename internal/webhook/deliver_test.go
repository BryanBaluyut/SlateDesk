package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignBodyMatchesReferenceHMAC pins the signature format a receiver must
// reproduce: "sha256=" + hex(HMAC-SHA256(secret, body)).
func TestSignBodyMatchesReferenceHMAC(t *testing.T) {
	secret := []byte("s3cr3t-signing-key")
	body := []byte(`{"event":"ticket.created"}`)

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if got := SignBody(secret, body); got != want {
		t.Fatalf("SignBody = %q, want %q", got, want)
	}
	// A different secret or body must produce a different signature.
	if SignBody([]byte("other"), body) == want {
		t.Errorf("signature did not change with a different secret")
	}
	if SignBody(secret, []byte(`{"event":"ticket.updated"}`)) == want {
		t.Errorf("signature did not change with a different body")
	}
}

// TestDeliverSignsBodyAndReportsSuccess verifies the exact bytes POSTed carry
// a signature the receiver can verify against the raw body, and a 2xx maps to
// (code, nil).
func TestDeliverSignsBodyAndReportsSuccess(t *testing.T) {
	secret := []byte("top-secret-key")
	body := []byte(`{"event":"ticket.created","ticket_number":"20260718-0001"}`)

	var gotSig, gotBody, gotCT, gotUA, gotDelivery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get(SignatureHeader)
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		gotDelivery = r.Header.Get(DeliveryHeader)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	code, err := Deliver(context.Background(), srv.Client(), srv.URL, 4242, secret, body)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if gotBody != string(body) {
		t.Errorf("sink received body %q, want %q", gotBody, body)
	}
	// The delivery id rides in a stable header so receivers can dedupe
	// at-least-once retries.
	if gotDelivery != "4242" {
		t.Errorf("delivery header = %q, want %q", gotDelivery, "4242")
	}
	// The signature the receiver recomputes over the body it read must match
	// the header — the core contract of HMAC webhook verification.
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(gotBody))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); gotSig != want {
		t.Errorf("signature %q does not verify against received body (want %q)", gotSig, want)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if !strings.HasPrefix(gotUA, "SlateDesk-Webhook") {
		t.Errorf("user-agent = %q, want SlateDesk-Webhook/*", gotUA)
	}
}

// TestDeliverNon2xxIsError: a non-2xx endpoint response yields (code, error)
// so the worker retries.
func TestDeliverNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	code, err := Deliver(context.Background(), srv.Client(), srv.URL, 1, []byte("k"), []byte("{}"))
	if err == nil {
		t.Fatalf("Deliver to a 500 endpoint returned nil error")
	}
	if code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", code)
	}
}

// TestDeliverTransportErrorHasZeroCode: an unreachable endpoint yields code 0
// (no HTTP response) plus an error.
func TestDeliverTransportErrorHasZeroCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // the address is now unreachable

	code, err := Deliver(context.Background(), &http.Client{Timeout: time.Second}, url, 1, []byte("k"), []byte("{}"))
	if err == nil {
		t.Fatalf("Deliver to a closed server returned nil error")
	}
	if code != 0 {
		t.Fatalf("transport-error code = %d, want 0", code)
	}
}
