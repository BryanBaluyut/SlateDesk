package handlers_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/db"
	"github.com/BryanBaluyut/slatedesk/internal/email"
	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/handlers"
	"github.com/BryanBaluyut/slatedesk/internal/jobs"
	"github.com/BryanBaluyut/slatedesk/internal/migrate"
	"github.com/BryanBaluyut/slatedesk/internal/secrets"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// mustUUID unwraps an API UUID (openapi_types.UUID is a uuid.UUID) for store
// calls. It is a test helper shared across the M4 handler tests.
func mustUUID(t *testing.T, id openapi_types.UUID) uuid.UUID {
	t.Helper()
	return uuid.UUID(id)
}

// authHeader returns an Authorization: Bearer header pair for e.do.
func authHeader(token string) [2]string {
	return [2]string{"Authorization", "Bearer " + token}
}

// --- Setup wizard (isolated fresh database) ---------------------------------

// freshEnv provisions a brand-new throwaway database (no admin, setup
// incomplete) and a full handler stack over it, so the first-run installer
// flow can be exercised end to end without touching the shared test database.
func freshEnv(t *testing.T) *env {
	t.Helper()
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		baseURL = defaultDatabaseURL
	}
	ctx := context.Background()

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("db suffix: %v", err)
	}
	dbName := fmt.Sprintf("slatedesk_setup_%d_%s", os.Getpid(), hex.EncodeToString(suffix[:]))

	adminConn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := adminConn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("create fresh db: %v", err)
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	parsed.Path = "/" + dbName
	freshURL := parsed.String()

	poolCfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	poolCfg.ConnConfig.Database = dbName
	poolCfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		db.RegisterTypes(conn)
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connect fresh pool: %v", err)
	}
	if err := migrate.Run(ctx, pool); err != nil {
		t.Fatalf("migrate fresh db: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		dropCtx := context.Background()
		if _, err := adminConn.Exec(dropCtx, "DROP DATABASE "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop fresh db %s: %v", dbName, err)
		}
		_ = adminConn.Close(dropCtx)
	})

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("secret: %v", err)
	}
	hub := events.NewHub()
	listenCtx, stopListener := context.WithCancel(context.Background())
	t.Cleanup(stopListener)
	go func() { _ = events.NewListener(freshURL, hub).Run(listenCtx) }()

	blobs, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	box, err := secrets.NewBox(secret, secrets.PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	engine := email.NewEngine(pool, email.NewAuth(pool, box), blobs)
	jc, err := jobs.NewClient(pool, nil)
	if err != nil {
		t.Fatalf("jobs client: %v", err)
	}
	engine.SetRiver(jc.River())

	h, err := handlers.New(pool, secret, auth.CookieSecureAuto, hub, blobs, engine, nil)
	if err != nil {
		t.Fatalf("handlers: %v", err)
	}
	r := chi.NewRouter()
	r.Route("/api", func(a chi.Router) { a.Mount("/v1", h.Router()) })
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &env{
		t: t, srv: srv, q: store.New(pool), svc: ticket.NewService(pool),
		hub: hub, secret: secret, engine: engine, box: box,
	}
}

func TestSetupWizardFlow(t *testing.T) {
	e := freshEnv(t)

	// Status before anything: not complete, token required.
	var st api.SetupStatus
	res := e.do(http.MethodGet, "/api/v1/setup/status", nil, nil)
	if res.status != http.StatusOK {
		t.Fatalf("setup/status: %d %s", res.status, res.body)
	}
	e.decode(res, &st)
	if st.Completed || !st.NeedsToken {
		t.Fatalf("fresh status = %+v, want {completed:false needs_token:true}", st)
	}

	// The installer gate: other /api/v1 routes 503 while setup is incomplete.
	if res := e.do(http.MethodGet, "/api/v1/tickets", nil, nil); res.status != http.StatusServiceUnavailable {
		t.Fatalf("gated GET /tickets: %d %s (want 503)", res.status, res.body)
	}

	token, err := handlers.GenerateSetupToken(e.secret)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	// A bad token is 401 (the route is exempt from the gate, but token-gated).
	if res := e.do(http.MethodPost, "/api/v1/setup/admin", nil, map[string]string{
		"token": "nope", "name": "A", "email": "a@example.com", "password": "supersecret1",
	}); res.status != http.StatusUnauthorized {
		t.Fatalf("bad-token setup/admin: %d %s (want 401)", res.status, res.body)
	}

	// Create the first admin.
	res = e.do(http.MethodPost, "/api/v1/setup/admin", nil, map[string]string{
		"token": token, "name": "Admin", "email": "admin@setup.test", "password": "supersecret1",
	})
	if res.status != http.StatusOK {
		t.Fatalf("setup/admin: %d %s (want 200)", res.status, res.body)
	}
	var cookie *http.Cookie
	for _, c := range res.cookies {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("setup/admin did not set a session cookie")
	}

	// Token now invalidated: a second create is 409 (an admin exists).
	if res := e.do(http.MethodPost, "/api/v1/setup/admin", nil, map[string]string{
		"token": token, "name": "B", "email": "b@setup.test", "password": "supersecret1",
	}); res.status != http.StatusConflict {
		t.Fatalf("second setup/admin: %d %s (want 409)", res.status, res.body)
	}

	// needs_token is now false; completed still false.
	e.decode(e.do(http.MethodGet, "/api/v1/setup/status", nil, nil), &st)
	if st.Completed || st.NeedsToken {
		t.Fatalf("post-admin status = %+v, want {completed:false needs_token:false}", st)
	}
	// Gate is still closed until /setup/complete.
	if res := e.do(http.MethodGet, "/api/v1/tickets", cookie, nil); res.status != http.StatusServiceUnavailable {
		t.Fatalf("gated GET /tickets after admin: %d (want 503)", res.status)
	}

	// Step 2: instance settings (normalized).
	res = e.do(http.MethodPost, "/api/v1/setup/instance", cookie, map[string]string{
		"name": "Acme Support", "external_url": "https://desk.acme.test/",
	})
	if res.status != http.StatusOK {
		t.Fatalf("setup/instance: %d %s", res.status, res.body)
	}
	var inst api.InstanceSettings
	e.decode(res, &inst)
	if inst.ExternalUrl != "https://desk.acme.test" {
		t.Fatalf("external_url = %q, want trailing slash stripped", inst.ExternalUrl)
	}

	// Step 3: complete — seeds a welcome ticket.
	res = e.do(http.MethodPost, "/api/v1/setup/complete", cookie, nil)
	if res.status != http.StatusOK {
		t.Fatalf("setup/complete: %d %s", res.status, res.body)
	}
	var done api.SetupCompleteResult
	e.decode(res, &done)
	if !done.Completed || done.WelcomeTicketNumber == nil || *done.WelcomeTicketNumber == "" {
		t.Fatalf("complete result = %+v, want completed + welcome ticket number", done)
	}

	// Completing twice is 409.
	if res := e.do(http.MethodPost, "/api/v1/setup/complete", cookie, nil); res.status != http.StatusConflict {
		t.Fatalf("second setup/complete: %d (want 409)", res.status)
	}

	// Gate is now open, and the welcome ticket is in the queue.
	e.decode(e.do(http.MethodGet, "/api/v1/setup/status", nil, nil), &st)
	if !st.Completed {
		t.Fatalf("final status completed = false")
	}
	res = e.do(http.MethodGet, "/api/v1/tickets", cookie, nil)
	if res.status != http.StatusOK {
		t.Fatalf("post-setup GET /tickets: %d %s (want 200)", res.status, res.body)
	}
	var list api.TicketList
	e.decode(res, &list)
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].Number != *done.WelcomeTicketNumber {
		t.Fatalf("welcome ticket not seeded/visible: total=%d items=%d", list.Total, len(list.Items))
	}
}

// --- Portal isolation (hard security boundary) ------------------------------

func TestPortalIsolation(t *testing.T) {
	e := newEnv(t)
	custA := e.seedUser(store.UserRoleCustomer)
	custB := e.seedUser(store.UserRoleCustomer)
	agent := e.seedUser(store.UserRoleAgent)
	admin := e.seedUser(store.UserRoleAdmin)
	cookieA := e.mustLogin(custA.Email, seedPassword)
	cookieB := e.mustLogin(custB.Email, seedPassword)
	cookieAgent := e.mustLogin(agent.Email, seedPassword)
	cookieAdmin := e.mustLogin(admin.Email, seedPassword)

	// A opens a ticket.
	res := e.do(http.MethodPost, "/api/v1/portal/tickets", cookieA, map[string]string{
		"subject": "My laptop is broken", "body": "It will not power on.",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("portal create: %d %s", res.status, res.body)
	}
	var created api.PortalTicketDetail
	e.decode(res, &created)
	ticketID := mustUUID(t, created.Id)

	// An agent adds an INTERNAL note (must never reach the customer).
	if _, err := e.svc.AddArticle(context.Background(), ticketID, ticket.ArticleInput{
		AuthorID:   &agent.ID,
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		IsInternal: true,
		BodyText:   "INTERNAL: escalate to hardware team",
	}); err != nil {
		t.Fatalf("seed internal note: %v", err)
	}

	// A sees its own ticket — but not the internal note.
	res = e.do(http.MethodGet, "/api/v1/portal/tickets/"+ticketID.String(), cookieA, nil)
	if res.status != http.StatusOK {
		t.Fatalf("A get own: %d %s", res.status, res.body)
	}
	var detail api.PortalTicketDetail
	e.decode(res, &detail)
	for _, a := range detail.Articles {
		if a.BodyText == "INTERNAL: escalate to hardware team" {
			t.Fatalf("portal leaked an internal note to the customer")
		}
	}
	if len(detail.Articles) != 1 {
		t.Fatalf("portal detail articles = %d, want 1 public", len(detail.Articles))
	}

	// B cannot GET or reply to A's ticket → 404 (NOT 403 — anti-enumeration).
	if res := e.do(http.MethodGet, "/api/v1/portal/tickets/"+ticketID.String(), cookieB, nil); res.status != http.StatusNotFound {
		t.Fatalf("B get A's ticket: %d (want 404)", res.status)
	}
	if res := e.do(http.MethodPost, "/api/v1/portal/tickets/"+ticketID.String()+"/reply", cookieB,
		map[string]string{"body": "let me in"}); res.status != http.StatusNotFound {
		t.Fatalf("B reply to A's ticket: %d (want 404)", res.status)
	}

	// A can reply; a waiting ticket flips back to open on the customer reply.
	if res := e.do(http.MethodPost, "/api/v1/portal/tickets/"+ticketID.String()+"/reply", cookieA,
		map[string]string{"body": "still broken"}); res.status != http.StatusCreated {
		t.Fatalf("A reply: %d %s", res.status, res.body)
	}

	// B has its own ticket; A's list must not include it.
	resB := e.do(http.MethodPost, "/api/v1/portal/tickets", cookieB, map[string]string{
		"subject": "B ticket", "body": "hello",
	})
	var bTicket api.PortalTicketDetail
	e.decode(resB, &bTicket)
	res = e.do(http.MethodGet, "/api/v1/portal/tickets", cookieA, nil)
	var aList []api.PortalTicket
	e.decode(res, &aList)
	for _, tk := range aList {
		if tk.Id == bTicket.Id {
			t.Fatalf("A's portal list leaked B's ticket")
		}
	}
	if len(aList) != 1 || aList[0].Id != created.Id {
		t.Fatalf("A's list = %d tickets, want exactly its own", len(aList))
	}

	// Agents and admins are refused the portal (403) — they use the workspace.
	if res := e.do(http.MethodGet, "/api/v1/portal/tickets", cookieAgent, nil); res.status != http.StatusForbidden {
		t.Fatalf("agent on portal: %d (want 403)", res.status)
	}
	if res := e.do(http.MethodGet, "/api/v1/portal/tickets", cookieAdmin, nil); res.status != http.StatusForbidden {
		t.Fatalf("admin on portal: %d (want 403)", res.status)
	}
	// And an unauthenticated portal request is 401.
	if res := e.do(http.MethodGet, "/api/v1/portal/tickets", nil, nil); res.status != http.StatusUnauthorized {
		t.Fatalf("anon on portal: %d (want 401)", res.status)
	}
}

// TestPortalReplyReopensClosedTicket pins the architecture doc §3 behavior:
// a customer portal reply to a CLOSED ticket reopens it (mirroring the email
// channel), so the reply is not silently buried with the ticket never
// resurfacing in staff views.
func TestPortalReplyReopensClosedTicket(t *testing.T) {
	e := newEnv(t)
	cust := e.seedUser(store.UserRoleCustomer)
	cookie := e.mustLogin(cust.Email, seedPassword)

	res := e.do(http.MethodPost, "/api/v1/portal/tickets", cookie, map[string]string{
		"subject": "printer offline", "body": "the office printer is offline",
	})
	if res.status != http.StatusCreated {
		t.Fatalf("portal create: %d %s", res.status, res.body)
	}
	var created api.PortalTicketDetail
	e.decode(res, &created)
	ticketID := mustUUID(t, created.Id)

	// Staff closes the ticket.
	if _, err := e.svc.UpdateStatus(context.Background(), ticketID, store.TicketStatusClosed, nil); err != nil {
		t.Fatalf("close ticket: %v", err)
	}

	// The customer replies from the portal on the now-closed ticket.
	if res := e.do(http.MethodPost, "/api/v1/portal/tickets/"+ticketID.String()+"/reply", cookie,
		map[string]string{"body": "still offline"}); res.status != http.StatusCreated {
		t.Fatalf("portal reply on closed ticket: %d %s (want 201)", res.status, res.body)
	}

	// The reply must have reopened the ticket.
	res = e.do(http.MethodGet, "/api/v1/portal/tickets/"+ticketID.String(), cookie, nil)
	if res.status != http.StatusOK {
		t.Fatalf("get ticket after reopen: %d %s", res.status, res.body)
	}
	var detail api.PortalTicketDetail
	e.decode(res, &detail)
	if detail.Status != api.TicketStatusOpen {
		t.Fatalf("closed ticket status after portal reply = %q, want open", detail.Status)
	}
}

// --- Public web form --------------------------------------------------------

func TestPublicForm(t *testing.T) {
	e := newEnv(t)

	// A normal submission creates a ticket and returns its number.
	res := e.do(http.MethodPost, "/api/v1/public/tickets", nil, map[string]string{
		"subject": "Website down", "body": "500 errors", "email": "visitor@example.test", "name": "Vic",
	})
	if res.status != http.StatusAccepted {
		t.Fatalf("public submit: %d %s (want 202)", res.status, res.body)
	}
	var acc api.PublicTicketAccepted
	e.decode(res, &acc)
	if acc.TicketNumber == nil || *acc.TicketNumber == "" {
		t.Fatalf("public submit returned no ticket number")
	}

	// The honeypot is silently dropped: still 202, but no ticket number.
	res = e.do(http.MethodPost, "/api/v1/public/tickets", nil, map[string]any{
		"subject": "spam", "body": "spam", "email": "bot@example.test", "_honeypot": "i am a bot",
	})
	if res.status != http.StatusAccepted {
		t.Fatalf("honeypot submit: %d (want 202)", res.status)
	}
	var dropped api.PublicTicketAccepted
	e.decode(res, &dropped)
	if dropped.TicketNumber != nil {
		t.Fatalf("honeypot submission created a ticket (%v)", *dropped.TicketNumber)
	}

	// No enumeration: a repeat email (existing user) is indistinguishable from
	// a new one — both are 202 with a number.
	res = e.do(http.MethodPost, "/api/v1/public/tickets", nil, map[string]string{
		"subject": "again", "body": "second issue", "email": "visitor@example.test",
	})
	e.decode(res, &acc)
	if res.status != http.StatusAccepted || acc.TicketNumber == nil {
		t.Fatalf("repeat-email submit: %d ticket=%v (want 202 with number)", res.status, acc.TicketNumber)
	}

	// The auto-created requester is a CUSTOMER (never agent/admin).
	u, err := e.q.GetUserByEmail(context.Background(), "visitor@example.test")
	if err != nil {
		t.Fatalf("load auto-created requester: %v", err)
	}
	if u.Role != store.UserRoleCustomer {
		t.Fatalf("auto-created requester role = %s, want customer", u.Role)
	}
}

func TestPublicFormRateLimit(t *testing.T) {
	e := newEnv(t)
	body := map[string]string{"subject": "s", "body": "b", "email": "rl@example.test"}
	// The per-IP burst is publicFormBurst (5); all requests share 127.0.0.1.
	for i := 0; i < 5; i++ {
		if res := e.do(http.MethodPost, "/api/v1/public/tickets", nil, body); res.status != http.StatusAccepted {
			t.Fatalf("submission %d: %d %s (want 202)", i, res.status, res.body)
		}
	}
	if res := e.do(http.MethodPost, "/api/v1/public/tickets", nil, body); res.status != http.StatusTooManyRequests {
		t.Fatalf("burst+1 submission: %d (want 429)", res.status)
	}
}

// --- API key authentication + scope gate ------------------------------------

func TestAPIKeyAuth(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)

	// Mint a read-only key and a write key.
	readKey := createKey(t, e, cookie, "read-only", []string{"read"})
	writeKey := createKey(t, e, cookie, "read-write", []string{"read", "write"})

	// A read key authenticates a GET (acts as the admin who minted it).
	if res := e.do(http.MethodGet, "/api/v1/tickets", nil, nil, authHeader(readKey.Key)); res.status != http.StatusOK {
		t.Fatalf("read key GET /tickets: %d %s (want 200)", res.status, res.body)
	}
	// A read key is refused (403) on a mutating call (scope gate).
	if res := e.do(http.MethodPost, "/api/v1/tags", nil, map[string]string{"name": uniqueName("t")}, authHeader(readKey.Key)); res.status != http.StatusForbidden {
		t.Fatalf("read key POST /tags: %d (want 403)", res.status)
	}
	// A write key may mutate.
	if res := e.do(http.MethodPost, "/api/v1/tags", nil, map[string]string{"name": uniqueName("t")}, authHeader(writeKey.Key)); res.status != http.StatusCreated {
		t.Fatalf("write key POST /tags: %d %s (want 201)", res.status, res.body)
	}
	// A malformed bearer token is 401.
	if res := e.do(http.MethodGet, "/api/v1/tickets", nil, nil, authHeader("sd_live_bogus")); res.status != http.StatusUnauthorized {
		t.Fatalf("bogus key: %d (want 401)", res.status)
	}

	// Revoking the read key stops it authenticating (401).
	if res := e.do(http.MethodDelete, "/api/v1/api-keys/"+mustUUID(t, readKey.Id).String(), cookie, nil); res.status != http.StatusNoContent {
		t.Fatalf("revoke read key: %d %s (want 204)", res.status, res.body)
	}
	if res := e.do(http.MethodGet, "/api/v1/tickets", nil, nil, authHeader(readKey.Key)); res.status != http.StatusUnauthorized {
		t.Fatalf("revoked key GET: %d (want 401)", res.status)
	}
}

func createKey(t *testing.T, e *env, cookie *http.Cookie, name string, scopes []string) api.ApiKeyCreated {
	t.Helper()
	res := e.do(http.MethodPost, "/api/v1/api-keys", cookie, map[string]any{"name": name, "scopes": scopes})
	if res.status != http.StatusCreated {
		t.Fatalf("create key %q: %d %s", name, res.status, res.body)
	}
	var k api.ApiKeyCreated
	e.decode(res, &k)
	if k.Key == "" {
		t.Fatalf("create key %q: empty plaintext", name)
	}
	return k
}
