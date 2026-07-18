package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/email"
	"github.com/BryanBaluyut/slatedesk/internal/settings"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// --- fixtures ---------------------------------------------------------------

// Distinctive credential strings: anything that must NEVER appear in a
// response body. Deliberately weird so an accidental echo cannot hide in
// legitimate response text.
const (
	secretBasicPassword  = "hunter2-BASIC-p4ssw0rd!"
	secretBasicRotated   = "rotated-BASIC-p4ssw0rd9"
	secretM365Secret     = "m365-cl13nt-S3CR3T~value"
	secretGoogleSecret   = "GOCSPX-g00gle-secret-value"
	secretRefreshToken   = "1//refresh-t0ken-VALUE"
	secretAccessToken    = "ya29.cached-access-t0ken"
	secretWrongPassword  = "wr0ng-greenmail-password"
	allKindsClientID     = "client-id-not-secret.apps.example"
	allKindsTenantID     = "tenant-id-not-secret-1234"
	googleOauthNoncePath = "/api/v1/mailboxes/oauth/google/callback"
)

// leakables returns every string that must never leave the server.
func leakables() []string {
	return []string{
		secretBasicPassword, secretBasicRotated, secretM365Secret,
		secretGoogleSecret, secretRefreshToken, secretAccessToken,
		secretWrongPassword,
	}
}

// assertNoSecrets fails if any credential string appears in body.
func assertNoSecrets(t *testing.T, where string, body []byte) {
	t.Helper()
	for _, s := range leakables() {
		if strings.Contains(string(body), s) {
			t.Fatalf("%s: response leaks credential %q: %s", where, s, body)
		}
	}
}

// basicMailboxBody is a valid basic-auth create payload. Host/ports point
// at a closed local port so accidental dials fail fast.
func basicMailboxBody(emailAddr string) map[string]any {
	return map[string]any{
		"auth_kind":     "basic",
		"name":          uniqueName("Support"),
		"email_address": emailAddr,
		"imap_host":     "127.0.0.1",
		"imap_port":     1,
		"imap_tls_mode": "none",
		"imap_username": "support",
		"smtp_host":     "127.0.0.1",
		"smtp_port":     1,
		"smtp_tls_mode": "none",
		"smtp_username": "support",
		"password":      secretBasicPassword,
	}
}

// seedMailbox inserts a mailbox directly via the store (bypassing the API
// under test), credentials encrypted with the env's box.
func (e *env) seedMailbox(kind store.MailboxAuthKind, emailAddr string, creds email.Credentials) store.Mailbox {
	e.t.Helper()
	enc, err := email.EncryptCredentials(e.box, creds)
	if err != nil {
		e.t.Fatalf("encrypt seed credentials: %v", err)
	}
	params := store.CreateMailboxParams{
		Name:           uniqueName("Seed Mailbox"),
		EmailAddress:   emailAddr,
		Active:         true,
		AuthKind:       kind,
		ImapHost:       "127.0.0.1",
		ImapPort:       1,
		ImapTlsMode:    store.MailTlsModeNone,
		ImapUsername:   "support",
		SmtpHost:       "127.0.0.1",
		SmtpPort:       1,
		SmtpTlsMode:    store.MailTlsModeNone,
		SmtpUsername:   "support",
		CredentialsEnc: enc,
		AutoAckEnabled: true,
	}
	if kind == store.MailboxAuthKindOauthM365 {
		params.OauthTenantID = pgtype.Text{String: allKindsTenantID, Valid: true}
	}
	if kind != store.MailboxAuthKindBasic {
		params.OauthClientID = pgtype.Text{String: allKindsClientID, Valid: true}
	}
	mb, err := e.q.CreateMailbox(context.Background(), params)
	if err != nil {
		e.t.Fatalf("seed mailbox: %v", err)
	}
	return mb
}

// decryptMailboxCreds re-reads the row and opens its credential blob.
func (e *env) decryptMailboxCreds(id uuid.UUID) email.Credentials {
	e.t.Helper()
	mb, err := e.q.GetMailbox(context.Background(), id)
	if err != nil {
		e.t.Fatalf("load mailbox %s: %v", id, err)
	}
	creds, err := email.DecryptCredentials(e.box, mb.CredentialsEnc)
	if err != nil {
		e.t.Fatalf("decrypt mailbox %s credentials: %v", id, err)
	}
	return creds
}

// doNoRedirect performs a request without following redirects (the OAuth
// callback answers 303; the SPA route it points at is not mounted here).
func (e *env) doNoRedirect(method, path string, cookie *http.Cookie) result {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatalf("read response body: %v", err)
	}
	return result{status: res.StatusCode, header: res.Header, body: raw, cookies: res.Cookies()}
}

// --- authorization ----------------------------------------------------------

// TestMailboxAuthMatrix: the whole /mailboxes + /settings surface is admin
// only — customers AND agents get 403 (anon 401); retry-send follows the
// ticket core (agent or admin).
func TestMailboxAuthMatrix(t *testing.T) {
	e := newEnv(t)
	adminU := e.seedUser(store.UserRoleAdmin)
	agentU := e.seedUser(store.UserRoleAgent)
	customerU := e.seedUser(store.UserRoleCustomer)

	cookies := map[string]*http.Cookie{
		"anon":     nil,
		"customer": e.mustLogin(customerU.Email, seedPassword),
		"agent":    e.mustLogin(agentU.Email, seedPassword),
		"admin":    e.mustLogin(adminU.Email, seedPassword),
	}
	principals := []string{"anon", "customer", "agent", "admin"}

	target := e.seedMailbox(store.MailboxAuthKindBasic, uniqueEmail(), email.Credentials{Password: secretBasicPassword})
	doomed := e.seedMailbox(store.MailboxAuthKindBasic, uniqueEmail(), email.Credentials{Password: secretBasicPassword})

	expect := func(anon, customer, agent, admin int) map[string]int {
		return map[string]int{"anon": anon, "customer": customer, "agent": agent, "admin": admin}
	}

	rows := []struct {
		name   string
		method string
		path   string
		body   any
		want   map[string]int
	}{
		{"list mailboxes", http.MethodGet, "/api/v1/mailboxes", nil,
			expect(401, 403, 403, 200)},
		{"create mailbox", http.MethodPost, "/api/v1/mailboxes", basicMailboxBody(uniqueEmail()),
			expect(401, 403, 403, 201)},
		{"get mailbox", http.MethodGet, "/api/v1/mailboxes/" + target.ID.String(), nil,
			expect(401, 403, 403, 200)},
		{"update mailbox", http.MethodPatch, "/api/v1/mailboxes/" + target.ID.String(),
			map[string]any{"name": uniqueName("Renamed")},
			expect(401, 403, 403, 200)},
		// A live dial to a closed local port: still HTTP 200 with ok=false
		// for the admin — the test ran.
		{"test fetch", http.MethodPost, "/api/v1/mailboxes/" + target.ID.String() + "/test-fetch", nil,
			expect(401, 403, 403, 200)},
		{"test send", http.MethodPost, "/api/v1/mailboxes/" + target.ID.String() + "/test-send",
			map[string]any{"to": "probe@example.com"},
			expect(401, 403, 403, 200)},
		// target is basic, so the admin outcome is 409 (not oauth_google) —
		// the point here is the 401/403 gating.
		{"google oauth start", http.MethodPost, "/api/v1/mailboxes/" + target.ID.String() + "/oauth/google/start", nil,
			expect(401, 403, 403, 409)},
		{"google oauth callback", http.MethodGet, "/api/v1/mailboxes/oauth/google/callback?state=garbage", nil,
			expect(401, 403, 403, 400)},
		{"get external url", http.MethodGet, "/api/v1/settings/external-url", nil,
			expect(401, 403, 403, 200)},
		{"set external url", http.MethodPut, "/api/v1/settings/external-url",
			map[string]any{"external_url": "https://desk.example.test"},
			expect(401, 403, 403, 200)},
		// Ticket-core rule: agents may retry; the id does not exist, so
		// authorized callers see 404.
		{"retry send", http.MethodPost, "/api/v1/articles/" + uuid.NewString() + "/retry-send", nil,
			expect(401, 403, 404, 404)},
		{"delete mailbox", http.MethodDelete, "/api/v1/mailboxes/" + doomed.ID.String(), nil,
			expect(401, 403, 403, 204)},
	}

	for _, row := range rows {
		for _, principal := range principals {
			t.Run(row.name+"/"+principal, func(t *testing.T) {
				res := e.do(row.method, row.path, cookies[principal], row.body)
				want := row.want[principal]
				if res.status != want {
					t.Fatalf("%s %s as %s: status %d, want %d; body %s",
						row.method, row.path, principal, res.status, want, res.body)
				}
				assertNoSecrets(t, row.name+"/"+principal, res.body)
				if want == 401 || want == 403 {
					if ct := res.header.Get("Content-Type"); ct != "application/problem+json" {
						t.Fatalf("rejection Content-Type = %q, want application/problem+json", ct)
					}
				}
			})
		}
	}
}

// --- CRUD + credential egress ----------------------------------------------

// TestMailboxCRUDAndCredentialEgress drives the full mailbox lifecycle for
// every auth kind and scans EVERY response body — success and error paths —
// for the credential strings. It also verifies the encryption-at-write and
// rotation semantics against the decrypted rows, and that every mutation
// kicks the supervisor.
func TestMailboxCRUDAndCredentialEgress(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)
	ctx := context.Background()

	check := func(where string, res result) result {
		t.Helper()
		assertNoSecrets(t, where, res.body)
		return res
	}

	// --- basic ---
	basicEmail := uniqueEmail()
	res := check("create basic", e.do(http.MethodPost, "/api/v1/mailboxes", cookie, basicMailboxBody(basicEmail)))
	if res.status != http.StatusCreated {
		t.Fatalf("create basic mailbox: status %d body %s", res.status, res.body)
	}
	if strings.Contains(string(res.body), `"password"`) || strings.Contains(string(res.body), `"credentials`) {
		t.Fatalf("create response carries a credential field: %s", res.body)
	}
	var created struct {
		Id            uuid.UUID `json:"id"`
		AuthKind      string    `json:"auth_kind"`
		EmailAddress  string    `json:"email_address"`
		OauthTenantId *string   `json:"oauth_tenant_id"`
		OauthClientId *string   `json:"oauth_client_id"`
		Active        bool      `json:"active"`
	}
	e.decode(res, &created)
	if created.AuthKind != "basic" || !created.Active || created.OauthClientId != nil {
		t.Fatalf("created mailbox body mismatch: %s", res.body)
	}
	if got := e.decryptMailboxCreds(created.Id).Password; got != secretBasicPassword {
		t.Fatalf("stored password = %q, want the submitted one", got)
	}
	kicksAfterCreate := e.kicker.kicks.Load()
	if kicksAfterCreate == 0 {
		t.Fatal("create mailbox did not kick the supervisor")
	}

	// Read paths.
	check("list", e.do(http.MethodGet, "/api/v1/mailboxes", cookie, nil))
	check("get", e.do(http.MethodGet, "/api/v1/mailboxes/"+created.Id.String(), cookie, nil))

	// Duplicate address -> 409, no secrets.
	res = check("dup create", e.do(http.MethodPost, "/api/v1/mailboxes", cookie, basicMailboxBody(basicEmail)))
	if res.status != http.StatusConflict {
		t.Fatalf("duplicate create: status %d body %s", res.status, res.body)
	}

	// Rotate the password via PATCH.
	res = check("rotate password", e.do(http.MethodPatch, "/api/v1/mailboxes/"+created.Id.String(), cookie,
		map[string]any{"password": secretBasicRotated}))
	if res.status != http.StatusOK {
		t.Fatalf("rotate password: status %d body %s", res.status, res.body)
	}
	if got := e.decryptMailboxCreds(created.Id).Password; got != secretBasicRotated {
		t.Fatalf("rotated password = %q, want the new one", got)
	}
	if e.kicker.kicks.Load() <= kicksAfterCreate {
		t.Fatal("update mailbox did not kick the supervisor")
	}

	// Live test endpoints against a dead port: 200 ok=false, detail free
	// of credentials.
	res = check("test-fetch dead port", e.do(http.MethodPost, "/api/v1/mailboxes/"+created.Id.String()+"/test-fetch", cookie, nil))
	if res.status != http.StatusOK {
		t.Fatalf("test-fetch: status %d body %s", res.status, res.body)
	}
	var tr struct {
		Ok        bool   `json:"ok"`
		Detail    string `json:"detail"`
		LatencyMs int64  `json:"latency_ms"`
	}
	e.decode(res, &tr)
	if tr.Ok || tr.Detail == "" || tr.LatencyMs < 0 {
		t.Fatalf("test-fetch against closed port: %+v", tr)
	}
	res = check("test-send dead port", e.do(http.MethodPost, "/api/v1/mailboxes/"+created.Id.String()+"/test-send", cookie,
		map[string]any{"to": "probe@example.com"}))
	if res.status != http.StatusOK {
		t.Fatalf("test-send: status %d body %s", res.status, res.body)
	}
	e.decode(res, &tr)
	if tr.Ok {
		t.Fatalf("test-send against closed port reported ok: %+v", tr)
	}

	// --- oauth_m365 ---
	m365Body := basicMailboxBody(uniqueEmail())
	delete(m365Body, "password")
	m365Body["auth_kind"] = "oauth_m365"
	m365Body["tenant_id"] = allKindsTenantID
	m365Body["client_id"] = allKindsClientID
	m365Body["client_secret"] = secretM365Secret
	res = check("create m365", e.do(http.MethodPost, "/api/v1/mailboxes", cookie, m365Body))
	if res.status != http.StatusCreated {
		t.Fatalf("create m365 mailbox: status %d body %s", res.status, res.body)
	}
	var m365 struct {
		Id            uuid.UUID `json:"id"`
		OauthTenantId *string   `json:"oauth_tenant_id"`
		OauthClientId *string   `json:"oauth_client_id"`
	}
	e.decode(res, &m365)
	if m365.OauthTenantId == nil || *m365.OauthTenantId != allKindsTenantID ||
		m365.OauthClientId == nil || *m365.OauthClientId != allKindsClientID {
		t.Fatalf("m365 ids not echoed (they are not secret): %s", res.body)
	}
	if got := e.decryptMailboxCreds(m365.Id).ClientSecret; got != secretM365Secret {
		t.Fatalf("stored m365 client secret = %q", got)
	}

	// Simulate a cached access token, then rotate the client secret: the
	// cache must be dropped.
	enc, err := email.EncryptCredentials(e.box, email.Credentials{
		ClientSecret: secretM365Secret,
		AccessToken:  secretAccessToken, AccessTokenExpiry: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := e.q.UpdateMailboxCredentials(ctx, store.UpdateMailboxCredentialsParams{ID: m365.Id, CredentialsEnc: enc}); err != nil {
		t.Fatalf("prime access token: %v", err)
	}
	res = check("rotate m365 secret", e.do(http.MethodPatch, "/api/v1/mailboxes/"+m365.Id.String(), cookie,
		map[string]any{"client_secret": secretM365Secret + "-rotated"}))
	if res.status != http.StatusOK {
		t.Fatalf("rotate m365 secret: status %d body %s", res.status, res.body)
	}
	if creds := e.decryptMailboxCreds(m365.Id); creds.AccessToken != "" || !creds.AccessTokenExpiry.IsZero() {
		t.Fatalf("rotating the client secret must drop the cached access token: %+v", creds)
	}

	// --- oauth_google ---
	gBody := basicMailboxBody(uniqueEmail())
	delete(gBody, "password")
	gBody["auth_kind"] = "oauth_google"
	gBody["client_id"] = allKindsClientID
	gBody["client_secret"] = secretGoogleSecret
	res = check("create google", e.do(http.MethodPost, "/api/v1/mailboxes", cookie, gBody))
	if res.status != http.StatusCreated {
		t.Fatalf("create google mailbox: status %d body %s", res.status, res.body)
	}
	var goog struct {
		Id            uuid.UUID `json:"id"`
		OauthTenantId *string   `json:"oauth_tenant_id"`
	}
	e.decode(res, &goog)
	if goog.OauthTenantId != nil {
		t.Fatalf("google mailbox must have null oauth_tenant_id: %s", res.body)
	}

	// Simulate a connected mailbox (refresh token present), then rotate the
	// client secret: the refresh token must be invalidated.
	enc, err = email.EncryptCredentials(e.box, email.Credentials{
		ClientSecret: secretGoogleSecret, RefreshToken: secretRefreshToken,
	})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := e.q.UpdateMailboxCredentials(ctx, store.UpdateMailboxCredentialsParams{ID: goog.Id, CredentialsEnc: enc}); err != nil {
		t.Fatalf("prime refresh token: %v", err)
	}
	res = check("rotate google secret", e.do(http.MethodPatch, "/api/v1/mailboxes/"+goog.Id.String(), cookie,
		map[string]any{"client_secret": secretGoogleSecret + "-rotated"}))
	if res.status != http.StatusOK {
		t.Fatalf("rotate google secret: status %d body %s", res.status, res.body)
	}
	if creds := e.decryptMailboxCreds(goog.Id); creds.RefreshToken != "" {
		t.Fatal("rotating an oauth_google client secret must invalidate the stored refresh token")
	}

	// --- auth_kind change ---
	// Missing new-kind credentials -> 400 (and no secrets in the error).
	res = check("kind change missing creds", e.do(http.MethodPatch, "/api/v1/mailboxes/"+goog.Id.String(), cookie,
		map[string]any{"auth_kind": "basic"}))
	if res.status != http.StatusBadRequest {
		t.Fatalf("kind change without password: status %d body %s", res.status, res.body)
	}
	// With the password supplied the switch succeeds and wipes the OAuth
	// material (columns and blob).
	res = check("kind change", e.do(http.MethodPatch, "/api/v1/mailboxes/"+goog.Id.String(), cookie,
		map[string]any{"auth_kind": "basic", "password": secretBasicRotated}))
	if res.status != http.StatusOK {
		t.Fatalf("kind change to basic: status %d body %s", res.status, res.body)
	}
	var switched struct {
		AuthKind      string  `json:"auth_kind"`
		OauthClientId *string `json:"oauth_client_id"`
	}
	e.decode(res, &switched)
	if switched.AuthKind != "basic" || switched.OauthClientId != nil {
		t.Fatalf("kind switch response mismatch: %s", res.body)
	}
	creds := e.decryptMailboxCreds(goog.Id)
	if creds.Password != secretBasicRotated || creds.ClientSecret != "" || creds.RefreshToken != "" {
		t.Fatalf("kind switch must reset the credential blob: %+v", creds)
	}

	// --- delete ---
	kicksBeforeDelete := e.kicker.kicks.Load()
	res = check("delete", e.do(http.MethodDelete, "/api/v1/mailboxes/"+goog.Id.String(), cookie, nil))
	if res.status != http.StatusNoContent {
		t.Fatalf("delete mailbox: status %d body %s", res.status, res.body)
	}
	if e.kicker.kicks.Load() <= kicksBeforeDelete {
		t.Fatal("delete mailbox did not kick the supervisor")
	}
	if res = e.do(http.MethodGet, "/api/v1/mailboxes/"+goog.Id.String(), cookie, nil); res.status != http.StatusNotFound {
		t.Fatalf("deleted mailbox still answers: status %d", res.status)
	}
	if res = e.do(http.MethodDelete, "/api/v1/mailboxes/"+goog.Id.String(), cookie, nil); res.status != http.StatusNotFound {
		t.Fatalf("double delete: status %d, want 404", res.status)
	}
}

// TestMailboxValidation: shape and auth_kind-specific input rejection.
func TestMailboxValidation(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)

	mutate := func(f func(m map[string]any)) map[string]any {
		m := basicMailboxBody(uniqueEmail())
		f(m)
		return m
	}

	createCases := []struct {
		name string
		body map[string]any
	}{
		// NOTE: a display-name form ("Support <s@example.com>") is not
		// rejected: the generated openapi_types.Email parses it down to
		// the bare addr-spec before the handler runs (same as user email
		// fields since M1).
		{"bad email", mutate(func(m map[string]any) { m["email_address"] = "not-an-email" })},
		{"port zero", mutate(func(m map[string]any) { m["imap_port"] = 0 })},
		{"port too big", mutate(func(m map[string]any) { m["smtp_port"] = 70000 })},
		{"empty name", mutate(func(m map[string]any) { m["name"] = "  " })},
		{"empty imap host", mutate(func(m map[string]any) { m["imap_host"] = "" })},
		{"bad tls mode", mutate(func(m map[string]any) { m["imap_tls_mode"] = "ssl3" })},
		{"missing password", mutate(func(m map[string]any) { delete(m, "password") })},
		{"empty password", mutate(func(m map[string]any) { m["password"] = "" })},
		{"unknown auth kind", mutate(func(m map[string]any) { m["auth_kind"] = "ldap" })},
		{"unknown field", mutate(func(m map[string]any) { m["surprise"] = true })},
		{"m365 missing tenant", mutate(func(m map[string]any) {
			delete(m, "password")
			m["auth_kind"] = "oauth_m365"
			m["client_id"] = allKindsClientID
			m["client_secret"] = secretM365Secret
		})},
		{"google missing client secret", mutate(func(m map[string]any) {
			delete(m, "password")
			m["auth_kind"] = "oauth_google"
			m["client_id"] = allKindsClientID
		})},
		{"basic with client_secret", mutate(func(m map[string]any) { m["client_secret"] = secretM365Secret })},
	}
	for _, tc := range createCases {
		t.Run("create/"+tc.name, func(t *testing.T) {
			res := e.do(http.MethodPost, "/api/v1/mailboxes", cookie, tc.body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body %s", res.status, res.body)
			}
			assertNoSecrets(t, tc.name, res.body)
		})
	}

	m365 := e.seedMailbox(store.MailboxAuthKindOauthM365, uniqueEmail(), email.Credentials{ClientSecret: secretM365Secret})
	goog := e.seedMailbox(store.MailboxAuthKindOauthGoogle, uniqueEmail(), email.Credentials{ClientSecret: secretGoogleSecret})
	basic := e.seedMailbox(store.MailboxAuthKindBasic, uniqueEmail(), email.Credentials{Password: secretBasicPassword})

	patchCases := []struct {
		name string
		id   uuid.UUID
		body map[string]any
	}{
		{"password on m365", m365.ID, map[string]any{"password": "irrelevant-pass"}},
		{"tenant on google", goog.ID, map[string]any{"tenant_id": "t"}},
		{"client secret on basic", basic.ID, map[string]any{"client_secret": "irrelevant-secret"}},
		{"empty name", basic.ID, map[string]any{"name": " "}},
		{"empty password", basic.ID, map[string]any{"password": ""}},
		{"bad port", basic.ID, map[string]any{"imap_port": -5}},
		{"unknown field", basic.ID, map[string]any{"surprise": 1}},
		{"kind to m365 missing ids", basic.ID, map[string]any{"auth_kind": "oauth_m365", "client_secret": "s"}},
	}
	for _, tc := range patchCases {
		t.Run("patch/"+tc.name, func(t *testing.T) {
			res := e.do(http.MethodPatch, "/api/v1/mailboxes/"+tc.id.String(), cookie, tc.body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400; body %s", res.status, res.body)
			}
			assertNoSecrets(t, tc.name, res.body)
		})
	}
}

// --- settings ---------------------------------------------------------------

func TestExternalUrlSetting(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)

	// The setting is a plain DB row shared across envs in this test
	// database; start from a clean slate.
	if err := settings.New(testPool).Delete(context.Background(), settings.KeyExternalURL); err != nil {
		t.Fatalf("clear external url: %v", err)
	}

	var got struct {
		ExternalUrl string `json:"external_url"`
	}
	res := e.do(http.MethodGet, "/api/v1/settings/external-url", cookie, nil)
	if res.status != http.StatusOK {
		t.Fatalf("get default: status %d body %s", res.status, res.body)
	}
	e.decode(res, &got)
	if got.ExternalUrl != "" {
		t.Fatalf("default external_url = %q, want empty", got.ExternalUrl)
	}

	for _, bad := range []string{
		"", "desk.example.com", "ftp://desk.example.com",
		"https://desk.example.com/help", "https://desk.example.com?x=1",
		"https://desk.example.com#frag", "https://user:pw@desk.example.com",
		"https://",
	} {
		res := e.do(http.MethodPut, "/api/v1/settings/external-url", cookie, map[string]any{"external_url": bad})
		if res.status != http.StatusBadRequest {
			t.Fatalf("PUT %q: status %d, want 400; body %s", bad, res.status, res.body)
		}
	}

	res = e.do(http.MethodPut, "/api/v1/settings/external-url", cookie,
		map[string]any{"external_url": "https://desk.example.test/"})
	if res.status != http.StatusOK {
		t.Fatalf("set external url: status %d body %s", res.status, res.body)
	}
	e.decode(res, &got)
	if got.ExternalUrl != "https://desk.example.test" {
		t.Fatalf("normalized external_url = %q, want trailing slash stripped", got.ExternalUrl)
	}
	res = e.do(http.MethodGet, "/api/v1/settings/external-url", cookie, nil)
	e.decode(res, &got)
	if got.ExternalUrl != "https://desk.example.test" {
		t.Fatalf("stored external_url = %q", got.ExternalUrl)
	}

	// http + port form is allowed (local/dev deployments).
	res = e.do(http.MethodPut, "/api/v1/settings/external-url", cookie,
		map[string]any{"external_url": "http://localhost:8000"})
	if res.status != http.StatusOK {
		t.Fatalf("set localhost external url: status %d body %s", res.status, res.body)
	}
}

// --- Google OAuth connect flow ----------------------------------------------

// TestGoogleOauthConnectFlow drives the whole browser round-trip against a
// local fake token endpoint: start mints a signed single-use state and the
// authorization URL; the callback validates the state (roundtrip, tamper,
// replay, cross-session binding), exchanges the code, and persists the
// refresh token encrypted.
func TestGoogleOauthConnectFlow(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	admin2 := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)
	cookie2 := e.mustLogin(admin2.Email, seedPassword)
	ctx := context.Background()

	if err := settings.New(testPool).Delete(ctx, settings.KeyExternalURL); err != nil {
		t.Fatalf("clear external url: %v", err)
	}

	goog := e.seedMailbox(store.MailboxAuthKindOauthGoogle, uniqueEmail(), email.Credentials{ClientSecret: secretGoogleSecret})
	basic := e.seedMailbox(store.MailboxAuthKindBasic, uniqueEmail(), email.Credentials{Password: secretBasicPassword})

	startPath := func(id uuid.UUID) string { return "/api/v1/mailboxes/" + id.String() + "/oauth/google/start" }

	// Preconditions: external URL unset -> 409; wrong kind -> 409.
	if res := e.do(http.MethodPost, startPath(goog.ID), cookie, nil); res.status != http.StatusConflict {
		t.Fatalf("start without external url: status %d body %s", res.status, res.body)
	}
	if res := e.do(http.MethodPut, "/api/v1/settings/external-url", cookie,
		map[string]any{"external_url": "https://desk.example.test"}); res.status != http.StatusOK {
		t.Fatalf("set external url: status %d body %s", res.status, res.body)
	}
	if res := e.do(http.MethodPost, startPath(basic.ID), cookie, nil); res.status != http.StatusConflict {
		t.Fatalf("start on basic mailbox: status %d body %s", res.status, res.body)
	}

	// Fake Google token endpoint. "good-code" succeeds; anything else
	// fails with an error body that (maliciously) embeds the client
	// secret, so the scrub path is exercised.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.FormValue("code") != "good-code" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"leaking %s on purpose"}`, secretGoogleSecret)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  secretAccessToken,
			"refresh_token": secretRefreshToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(tokenSrv.Close)
	e.engine.Auth().SetGoogleTokenURLForTest(tokenSrv.URL)

	startFlow := func(c *http.Cookie) (state string, authURL *url.URL) {
		t.Helper()
		res := e.do(http.MethodPost, startPath(goog.ID), c, nil)
		if res.status != http.StatusOK {
			t.Fatalf("oauth start: status %d body %s", res.status, res.body)
		}
		assertNoSecrets(t, "oauth start", res.body)
		var body struct {
			AuthorizationUrl string `json:"authorization_url"`
		}
		e.decode(res, &body)
		u, err := url.Parse(body.AuthorizationUrl)
		if err != nil {
			t.Fatalf("parse authorization_url %q: %v", body.AuthorizationUrl, err)
		}
		return u.Query().Get("state"), u
	}

	state, authURL := startFlow(cookie)
	if authURL.Host != "accounts.google.com" {
		t.Fatalf("authorization host = %q", authURL.Host)
	}
	q := authURL.Query()
	if got := q.Get("redirect_uri"); got != "https://desk.example.test"+googleOauthNoncePath {
		t.Fatalf("redirect_uri = %q", got)
	}
	if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Fatalf("authorization url missing offline/consent params: %s", authURL)
	}
	if q.Get("client_id") != allKindsClientID || state == "" {
		t.Fatalf("authorization url missing client_id/state: %s", authURL)
	}

	callback := func(c *http.Cookie, state, code string) result {
		path := "/api/v1/mailboxes/oauth/google/callback?state=" + url.QueryEscape(state)
		if code != "" {
			path += "&code=" + url.QueryEscape(code)
		}
		return e.doNoRedirect(http.MethodGet, path, c)
	}

	// Tampered state (flip a payload character) -> 400, nothing persisted.
	tampered := []byte(state)
	if tampered[0] == 'A' {
		tampered[0] = 'B'
	} else {
		tampered[0] = 'A'
	}
	if res := callback(cookie, string(tampered), "good-code"); res.status != http.StatusBadRequest {
		t.Fatalf("tampered state: status %d body %s", res.status, res.body)
	}

	// State bound to the initiating session: another admin's cookie -> 400
	// (checked before the nonce is consumed, so the state stays usable).
	if res := callback(cookie2, state, "good-code"); res.status != http.StatusBadRequest {
		t.Fatalf("cross-session state: status %d body %s", res.status, res.body)
	}

	// The genuine callback: 303 to the mailbox screen, refresh token
	// stored encrypted, updated_at bumped (so a supervisor reloads), and
	// a supervisor kick.
	before, err := e.q.GetMailbox(ctx, goog.ID)
	if err != nil {
		t.Fatalf("load mailbox: %v", err)
	}
	kicksBefore := e.kicker.kicks.Load()
	res := callback(cookie, state, "good-code")
	if res.status != http.StatusSeeOther {
		t.Fatalf("callback: status %d body %s", res.status, res.body)
	}
	assertNoSecrets(t, "oauth callback", res.body)
	if loc := res.header.Get("Location"); loc != "/settings/mailboxes?connected=1" {
		t.Fatalf("callback Location = %q", loc)
	}
	creds := e.decryptMailboxCreds(goog.ID)
	if creds.RefreshToken != secretRefreshToken || creds.AccessToken != secretAccessToken {
		t.Fatalf("callback did not persist tokens: %+v", creds)
	}
	after, err := e.q.GetMailbox(ctx, goog.ID)
	if err != nil {
		t.Fatalf("load mailbox: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatal("callback must bump updated_at so the supervisor reloads the mailbox")
	}
	if e.kicker.kicks.Load() <= kicksBefore {
		t.Fatal("callback did not kick the supervisor")
	}

	// Replay: the state is single-use.
	if res := callback(cookie, state, "good-code"); res.status != http.StatusBadRequest {
		t.Fatalf("replayed state: status %d body %s", res.status, res.body)
	}

	// Provider error arm: a fresh state with error=access_denied -> 400.
	state2, _ := startFlow(cookie)
	res = e.doNoRedirect(http.MethodGet,
		"/api/v1/mailboxes/oauth/google/callback?state="+url.QueryEscape(state2)+"&error=access_denied", cookie)
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), "access_denied") {
		t.Fatalf("provider error: status %d body %s", res.status, res.body)
	}

	// Exchange failure: the token endpoint's error body embeds the client
	// secret; the 400 problem must not.
	state3, _ := startFlow(cookie)
	res = callback(cookie, state3, "bad-code")
	if res.status != http.StatusBadRequest {
		t.Fatalf("failed exchange: status %d body %s", res.status, res.body)
	}
	assertNoSecrets(t, "failed exchange", res.body)
	if !strings.Contains(string(res.body), "[redacted]") {
		t.Fatalf("failed exchange should carry the scrubbed provider error: %s", res.body)
	}
}

// --- retry-send ---------------------------------------------------------------

func TestRetryArticleSend(t *testing.T) {
	e := newEnv(t)
	agent := e.seedUser(store.UserRoleAgent)
	customer := e.seedUser(store.UserRoleCustomer)
	cookie := e.mustLogin(agent.Email, seedPassword)
	ctx := context.Background()

	mb := e.seedMailbox(store.MailboxAuthKindBasic, uniqueEmail(), email.Credentials{Password: secretBasicPassword})
	tk := e.seedTicket(customer, uniqueName("retry"), "customer body", seedTicketOpts{})

	// Give the ticket an email origin so the retry can resolve a mailbox.
	if _, err := e.q.InsertEmailMessageID(ctx, store.InsertEmailMessageIDParams{
		MessageID: uniqueName("inbound") + "@example.test",
		Direction: store.EmailDirectionInbound,
		MailboxID: pgtype.UUID{Bytes: mb.ID, Valid: true},
		TicketID:  tk.ID,
	}); err != nil {
		t.Fatalf("insert inbound message id: %v", err)
	}

	// A terminally failed outbound reply.
	failed, err := e.q.CreateArticle(ctx, store.CreateArticleParams{
		TicketID:   tk.ID,
		AuthorID:   pgtype.UUID{Bytes: agent.ID, Valid: true},
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelEmail,
		BodyText:   "failed outbound reply",
	})
	if err != nil {
		t.Fatalf("seed failed article: %v", err)
	}
	if _, err := e.q.SetArticleEmailMeta(ctx, store.SetArticleEmailMetaParams{
		ID:             failed.ID,
		MessageID:      pgtype.Text{String: uniqueName("outbound") + "@example.test", Valid: true},
		DeliveryStatus: pgtype.Text{String: email.DeliveryFailed, Valid: true},
	}); err != nil {
		t.Fatalf("mark article failed: %v", err)
	}

	res := e.do(http.MethodPost, "/api/v1/articles/"+failed.ID.String()+"/retry-send", cookie, nil)
	if res.status != http.StatusAccepted {
		t.Fatalf("retry-send: status %d body %s", res.status, res.body)
	}
	var got struct {
		Id             uuid.UUID `json:"id"`
		DeliveryStatus *string   `json:"delivery_status"`
	}
	e.decode(res, &got)
	if got.Id != failed.ID || got.DeliveryStatus == nil || *got.DeliveryStatus != "queued" {
		t.Fatalf("retry-send body mismatch: %s", res.body)
	}

	// The email_send job committed with the status flip.
	var jobs int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM river_job WHERE kind = 'email_send'").Scan(&jobs); err != nil {
		t.Fatalf("count river jobs: %v", err)
	}
	if jobs == 0 {
		t.Fatal("retry-send did not enqueue an email_send job")
	}

	// No longer failed -> 409.
	if res := e.do(http.MethodPost, "/api/v1/articles/"+failed.ID.String()+"/retry-send", cookie, nil); res.status != http.StatusConflict {
		t.Fatalf("second retry: status %d, want 409; body %s", res.status, res.body)
	}

	// A plain web article (delivery_status null) -> 409.
	plain, err := e.q.CreateArticle(ctx, store.CreateArticleParams{
		TicketID:   tk.ID,
		AuthorID:   pgtype.UUID{Bytes: agent.ID, Valid: true},
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		BodyText:   "not an email",
	})
	if err != nil {
		t.Fatalf("seed plain article: %v", err)
	}
	if res := e.do(http.MethodPost, "/api/v1/articles/"+plain.ID.String()+"/retry-send", cookie, nil); res.status != http.StatusConflict {
		t.Fatalf("retry on non-email article: status %d, want 409; body %s", res.status, res.body)
	}
}

// --- live GreenMail dials -----------------------------------------------------

// TestMailboxLiveTestsAgainstGreenMail exercises test-fetch and test-send
// against the real GreenMail SMTP+IMAP server. Opt-in like the email
// package's live smoke test:
//
//	SLATEDESK_GREENMAIL_TEST=1 go test ./internal/handlers -run GreenMail -v
func TestMailboxLiveTestsAgainstGreenMail(t *testing.T) {
	if os.Getenv("SLATEDESK_GREENMAIL_TEST") == "" {
		t.Skip("set SLATEDESK_GREENMAIL_TEST=1 to run the live GreenMail test")
	}
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)

	body := map[string]any{
		"auth_kind":     "basic",
		"name":          uniqueName("GreenMail Support"),
		"email_address": "support@slatedesk.test",
		"imap_host":     "127.0.0.1",
		"imap_port":     3143,
		"imap_tls_mode": "none",
		"imap_username": "support", // GreenMail registers the short login name
		"smtp_host":     "127.0.0.1",
		"smtp_port":     3025,
		"smtp_tls_mode": "none",
		"smtp_username": "support",
		"password":      "secret",
	}
	res := e.do(http.MethodPost, "/api/v1/mailboxes", cookie, body)
	if res.status != http.StatusCreated {
		t.Fatalf("create greenmail mailbox: status %d body %s", res.status, res.body)
	}
	var mb struct {
		Id uuid.UUID `json:"id"`
	}
	e.decode(res, &mb)

	var tr struct {
		Ok        bool   `json:"ok"`
		Detail    string `json:"detail"`
		LatencyMs int64  `json:"latency_ms"`
	}

	res = e.do(http.MethodPost, "/api/v1/mailboxes/"+mb.Id.String()+"/test-fetch", cookie, nil)
	if res.status != http.StatusOK {
		t.Fatalf("test-fetch: status %d body %s", res.status, res.body)
	}
	e.decode(res, &tr)
	if !tr.Ok || tr.LatencyMs < 0 {
		t.Fatalf("live test-fetch failed: %+v", tr)
	}
	if !strings.Contains(tr.Detail, "INBOX") {
		t.Fatalf("test-fetch success detail = %q", tr.Detail)
	}

	res = e.do(http.MethodPost, "/api/v1/mailboxes/"+mb.Id.String()+"/test-send", cookie,
		map[string]any{"to": "customer@example.test"})
	if res.status != http.StatusOK {
		t.Fatalf("test-send: status %d body %s", res.status, res.body)
	}
	e.decode(res, &tr)
	if !tr.Ok {
		t.Fatalf("live test-send failed: %+v", tr)
	}

	// Wrong password against the real server: 200 ok=false, and the detail
	// must not echo the stored (wrong) password.
	wrong := e.seedMailbox(store.MailboxAuthKindBasic, "support-wrong@slatedesk.test",
		email.Credentials{Password: secretWrongPassword})
	if _, err := e.q.UpdateMailbox(context.Background(), store.UpdateMailboxParams{
		ID: wrong.ID, Name: wrong.Name, EmailAddress: wrong.EmailAddress, Active: true,
		AuthKind: wrong.AuthKind,
		ImapHost: "127.0.0.1", ImapPort: 3143, ImapTlsMode: store.MailTlsModeNone, ImapUsername: "support",
		SmtpHost: "127.0.0.1", SmtpPort: 3025, SmtpTlsMode: store.MailTlsModeNone, SmtpUsername: "support",
		CredentialsEnc: wrong.CredentialsEnc, FromDisplayName: wrong.FromDisplayName,
		Signature: wrong.Signature, AutoAckEnabled: wrong.AutoAckEnabled,
	}); err != nil {
		t.Fatalf("point mailbox at greenmail: %v", err)
	}
	res = e.do(http.MethodPost, "/api/v1/mailboxes/"+wrong.ID.String()+"/test-fetch", cookie, nil)
	if res.status != http.StatusOK {
		t.Fatalf("wrong-password test-fetch: status %d body %s", res.status, res.body)
	}
	e.decode(res, &tr)
	if tr.Ok {
		t.Fatal("test-fetch with a wrong password reported ok")
	}
	if tr.Detail == "" {
		t.Fatal("wrong-password test-fetch: empty detail")
	}
	assertNoSecrets(t, "wrong-password detail", res.body)
}
