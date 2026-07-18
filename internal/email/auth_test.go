package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// makeOAuthMailbox inserts a mailbox with the given OAuth auth kind and
// encrypted credentials.
func makeOAuthMailbox(t *testing.T, kind store.MailboxAuthKind, creds Credentials, tenant, clientID string) store.Mailbox {
	t.Helper()
	enc, err := EncryptCredentials(testBox, creds)
	if err != nil {
		t.Fatalf("encrypt credentials: %v", err)
	}
	addr := uniq("oauth") + "@slatedesk.test"
	mb, err := store.New(testPool).CreateMailbox(context.Background(), store.CreateMailboxParams{
		Name:            "OAuth mailbox " + addr,
		EmailAddress:    addr,
		Active:          true,
		AuthKind:        kind,
		ImapHost:        "127.0.0.1",
		ImapPort:        1,
		ImapTlsMode:     store.MailTlsModeNone,
		ImapUsername:    addr,
		SmtpHost:        "127.0.0.1",
		SmtpPort:        1,
		SmtpTlsMode:     store.MailTlsModeNone,
		SmtpUsername:    addr,
		CredentialsEnc:  enc,
		OauthTenantID:   pgtype.Text{String: tenant, Valid: tenant != ""},
		OauthClientID:   pgtype.Text{String: clientID, Valid: clientID != ""},
		FromDisplayName: "OAuth",
		AutoAckEnabled:  false,
	})
	if err != nil {
		t.Fatalf("create oauth mailbox: %v", err)
	}
	return mb
}

// decryptMailboxCreds reloads the mailbox row and decrypts its blob.
func decryptMailboxCreds(t *testing.T, id store.Mailbox) Credentials {
	t.Helper()
	mb, err := store.New(testPool).GetMailbox(context.Background(), id.ID)
	if err != nil {
		t.Fatalf("reload mailbox: %v", err)
	}
	raw, err := testBox.Decrypt(mb.CredentialsEnc)
	if err != nil {
		t.Fatalf("decrypt credentials: %v", err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode credentials: %v", err)
	}
	return c
}

// TestXOAUTH2SASLClient pins the wire format of the XOAUTH2 initial
// response (what both IMAP servers and, via go-mail, SMTP servers see).
func TestXOAUTH2SASLClient(t *testing.T) {
	c := newXOAUTH2Client("user@example.test", "tok-123")
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("mech = %q", mech)
	}
	want := "user=user@example.test\x01auth=Bearer tok-123\x01\x01"
	if string(ir) != want {
		t.Errorf("initial response = %q, want %q", ir, want)
	}
	// Failure path: server sends a JSON challenge; the client answers with
	// an empty response once, then aborts.
	resp, err := c.Next([]byte(`{"status":"401"}`))
	if err != nil || resp == nil || len(resp) != 0 {
		t.Errorf("first Next = (%q, %v), want empty response, nil error", resp, err)
	}
	if _, err := c.Next(nil); err == nil {
		t.Errorf("second Next must fail the exchange")
	}
}

// TestM365ClientCredentialsFlow: token minted via the client-credentials
// grant against a fake tenant endpoint, cached encrypted in the mailbox
// row, served from cache while fresh, and re-fetched when expired.
func TestM365ClientCredentialsFlow(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	var lastForm atomic.Value

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		lastForm.Store(r.Form.Encode() + "|path=" + r.URL.Path + "|auth=" + r.Header.Get("Authorization"))
		n := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"m365-tok-%d","token_type":"Bearer","expires_in":3600}`, n)
	}))
	defer ts.Close()

	mb := makeOAuthMailbox(t, store.MailboxAuthKindOauthM365,
		Credentials{ClientSecret: "shhh"}, "tenant-123", "client-abc")

	auth := NewAuth(testPool, testBox)
	auth.m365TokenURL = func(tenant string) string { return ts.URL + "/" + tenant + "/oauth2/v2.0/token" }

	tok, err := auth.accessToken(ctx, mb)
	if err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if tok != "m365-tok-1" {
		t.Errorf("token = %q", tok)
	}
	form, _ := lastForm.Load().(string)
	if !strings.Contains(form, "grant_type=client_credentials") {
		t.Errorf("grant type missing: %s", form)
	}
	if !strings.Contains(form, "outlook.office365.com%2F.default") {
		t.Errorf("scope missing: %s", form)
	}
	if !strings.Contains(form, "path=/tenant-123/oauth2/v2.0/token") {
		t.Errorf("tenant not in token URL: %s", form)
	}
	// x/oauth2 sends client id/secret via Basic auth by default; accept
	// either transport but require the client id somewhere.
	if !strings.Contains(form, "client-abc") && !strings.Contains(form, "auth=Basic") {
		t.Errorf("client credentials missing: %s", form)
	}

	// Token persisted encrypted (refresh-on-expiry cache).
	creds := decryptMailboxCreds(t, mb)
	if creds.AccessToken != "m365-tok-1" || creds.ClientSecret != "shhh" {
		t.Errorf("persisted creds = %+v", creds)
	}
	if !creds.AccessTokenExpiry.After(time.Now()) {
		t.Errorf("persisted expiry not in the future: %v", creds.AccessTokenExpiry)
	}

	// Cache hit: the mailbox row now carries the token; a fresh load does
	// not touch the IDP.
	mb2, err := store.New(testPool).GetMailbox(ctx, mb.ID)
	if err != nil {
		t.Fatalf("reload mailbox: %v", err)
	}
	if tok2, err := auth.accessToken(ctx, mb2); err != nil || tok2 != "m365-tok-1" {
		t.Fatalf("cached token = (%q, %v)", tok2, err)
	}
	if hits.Load() != 1 {
		t.Errorf("IDP hits = %d, want 1 (second call must be served from cache)", hits.Load())
	}

	// Expire the cached token: next call refreshes and re-persists.
	creds.AccessTokenExpiry = time.Now().Add(-time.Minute)
	enc, err := EncryptCredentials(testBox, creds)
	if err != nil {
		t.Fatalf("re-encrypt: %v", err)
	}
	if err := store.New(testPool).UpdateMailboxCredentials(ctx, store.UpdateMailboxCredentialsParams{
		ID: mb.ID, CredentialsEnc: enc,
	}); err != nil {
		t.Fatalf("store expired creds: %v", err)
	}
	mb3, _ := store.New(testPool).GetMailbox(ctx, mb.ID)
	tok3, err := auth.accessToken(ctx, mb3)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok3 != "m365-tok-2" || hits.Load() != 2 {
		t.Errorf("refreshed token = %q (hits %d), want m365-tok-2 (2)", tok3, hits.Load())
	}
	if got := decryptMailboxCreds(t, mb).AccessToken; got != "m365-tok-2" {
		t.Errorf("refresh not persisted: %q", got)
	}
}

// TestGoogleRefreshTokenFlow: access token minted from the stored refresh
// token; a rotated refresh token from the IDP is persisted.
func TestGoogleRefreshTokenFlow(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "rt-1" {
			t.Errorf("refresh_token = %q", got)
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Google rotates the refresh token here.
		fmt.Fprint(w, `{"access_token":"goog-tok-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`)
	}))
	defer ts.Close()

	mb := makeOAuthMailbox(t, store.MailboxAuthKindOauthGoogle,
		Credentials{ClientSecret: "gsecret", RefreshToken: "rt-1"}, "", "google-client")

	auth := NewAuth(testPool, testBox)
	auth.googleTokenURL = ts.URL + "/token"

	tok, err := auth.accessToken(ctx, mb)
	if err != nil {
		t.Fatalf("accessToken: %v", err)
	}
	if tok != "goog-tok-1" || hits.Load() != 1 {
		t.Errorf("token = %q (hits %d)", tok, hits.Load())
	}

	creds := decryptMailboxCreds(t, mb)
	if creds.AccessToken != "goog-tok-1" {
		t.Errorf("access token not persisted: %+v", creds)
	}
	if creds.RefreshToken != "rt-2" {
		t.Errorf("rotated refresh token not persisted: %q", creds.RefreshToken)
	}
}

// TestBasicIMAPSASL: the basic path yields a PLAIN client with the
// mailbox's IMAP username and decrypted password.
func TestBasicIMAPSASL(t *testing.T) {
	mb := makeMailbox(t, mailboxOpts{password: "hunter2"})
	auth := NewAuth(testPool, testBox)
	c, err := auth.IMAPSASL(context.Background(), mb)
	if err != nil {
		t.Fatalf("IMAPSASL: %v", err)
	}
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "PLAIN" {
		t.Errorf("mech = %q", mech)
	}
	want := "\x00" + mb.ImapUsername + "\x00hunter2"
	if string(ir) != want {
		t.Errorf("PLAIN ir = %q, want %q", ir, want)
	}
}

// TestCredentialsRoundTrip: encrypt/decrypt via the shared box.
func TestCredentialsRoundTrip(t *testing.T) {
	in := Credentials{Password: "p", ClientSecret: "cs", RefreshToken: "rt",
		AccessToken: "at", AccessTokenExpiry: time.Now().Add(time.Hour).Truncate(time.Second)}
	enc, err := EncryptCredentials(testBox, in)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, err := testBox.Decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var out Credentials
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Password != in.Password || out.RefreshToken != in.RefreshToken ||
		out.AccessToken != in.AccessToken || !out.AccessTokenExpiry.Equal(in.AccessTokenExpiry) {
		t.Errorf("round trip mismatch: %+v != %+v", out, in)
	}
}

// TestAccessTokenStaleSnapshot: accessToken must decide on the CURRENT
// persisted blob, not the caller's mailbox snapshot — the IMAP runner's
// row is loaded once at reconcile and token-cache writes deliberately do
// not restart it. A stale snapshot must (a) be served from the persisted
// token cache instead of re-minting per reconnect, and (b) refresh with
// the newest persisted refresh token, never writing the stale one back.
func TestAccessTokenStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		// (b): the refresh must use the rotated token persisted by "another
		// path" (SMTP worker / peer), not the stale snapshot's rt-1.
		if got := r.Form.Get("refresh_token"); got != "rt-2" {
			t.Errorf("refresh_token = %q, want rt-2 (stale snapshot used instead of fresh blob)", got)
		}
		n := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"stale-tok-%d","token_type":"Bearer","expires_in":3600}`, n)
	}))
	defer ts.Close()

	// Snapshot blob carries rt-1 and no cached access token.
	mb := makeOAuthMailbox(t, store.MailboxAuthKindOauthGoogle,
		Credentials{ClientSecret: "gsecret", RefreshToken: "rt-1"}, "", "google-client")

	// Another path rotated the refresh token in the DB; mb is now stale.
	enc, err := EncryptCredentials(testBox, Credentials{ClientSecret: "gsecret", RefreshToken: "rt-2"})
	if err != nil {
		t.Fatalf("encrypt rotated creds: %v", err)
	}
	if err := store.New(testPool).UpdateMailboxCredentials(ctx, store.UpdateMailboxCredentialsParams{
		ID: mb.ID, CredentialsEnc: enc,
	}); err != nil {
		t.Fatalf("persist rotated creds: %v", err)
	}

	auth := NewAuth(testPool, testBox)
	auth.googleTokenURL = ts.URL + "/token"

	if tok, err := auth.accessToken(ctx, mb); err != nil || tok != "stale-tok-1" {
		t.Fatalf("accessToken = (%q, %v), want stale-tok-1", tok, err)
	}
	// (a): the mint above persisted a cache; the SAME stale snapshot must
	// now be served from it — one IDP hit total, not one per reconnect.
	if tok, err := auth.accessToken(ctx, mb); err != nil || tok != "stale-tok-1" {
		t.Fatalf("second accessToken = (%q, %v), want cached stale-tok-1", tok, err)
	}
	if hits.Load() != 1 {
		t.Errorf("IDP hits = %d, want 1 (stale snapshot must hit the persisted cache)", hits.Load())
	}
	// The rotated refresh token survived the write-back.
	if got := decryptMailboxCreds(t, mb).RefreshToken; got != "rt-2" {
		t.Errorf("persisted refresh token = %q, want rt-2 (lost update)", got)
	}
}
