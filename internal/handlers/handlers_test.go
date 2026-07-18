package handlers_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/email"
	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/handlers"
	"github.com/BryanBaluyut/slatedesk/internal/jobs"
	"github.com/BryanBaluyut/slatedesk/internal/secrets"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

var uniqueCounter atomic.Int64

func uniqueEmail() string {
	return fmt.Sprintf("user%d@example.com", uniqueCounter.Add(1))
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, uniqueCounter.Add(1))
}

// fakeKicker counts supervisor pokes from mailbox mutations.
type fakeKicker struct{ kicks atomic.Int64 }

func (k *fakeKicker) Kick() { k.kicks.Add(1) }

// env is one isolated handler stack: its own instance secret (so cookies
// from other envs never verify), its own SSE hub fed by a dedicated
// LISTEN connection, its own temp-dir blob storage, and its own email
// engine with an insert-only River client (transactional enqueue works;
// no workers run) — mounted at /api/v1 like production.
type env struct {
	t      *testing.T
	srv    *httptest.Server
	q      *store.Queries
	svc    *ticket.Service
	hub    *events.Hub
	secret []byte
	engine *email.Engine
	box    *secrets.Box
	kicker *fakeKicker
}

func newEnv(t *testing.T) *env {
	t.Helper()
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatalf("generate secret: %v", err)
	}

	hub := events.NewHub()
	listenCtx, stopListener := context.WithCancel(context.Background())
	t.Cleanup(stopListener)
	go func() { _ = events.NewListener(testDatabaseURL, hub).Run(listenCtx) }()

	blobs, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create blob storage: %v", err)
	}

	// Same box derivation as handlers.New, so tests can decrypt what the
	// handlers stored.
	box, err := secrets.NewBox(secret, secrets.PurposeMailboxCredentials)
	if err != nil {
		t.Fatalf("create secrets box: %v", err)
	}
	engine := email.NewEngine(testPool, email.NewAuth(testPool, box), blobs)
	jc, err := jobs.NewClient(testPool, nil) // insert-only, like serve --no-worker
	if err != nil {
		t.Fatalf("create jobs client: %v", err)
	}
	engine.SetRiver(jc.River())
	kicker := &fakeKicker{}

	h, err := handlers.New(testPool, secret, auth.CookieSecureAuto, hub, blobs, engine, kicker)
	if err != nil {
		t.Fatalf("build handlers: %v", err)
	}
	r := chi.NewRouter()
	r.Route("/api", func(api chi.Router) {
		api.Mount("/v1", h.Router())
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &env{
		t:      t,
		srv:    srv,
		q:      store.New(testPool),
		svc:    ticket.NewService(testPool),
		hub:    hub,
		secret: secret,
		engine: engine,
		box:    box,
		kicker: kicker,
	}
}

// seedUser inserts a user with the shared seed password directly via the
// store (not the API, so tests do not depend on the endpoint under test).
func (e *env) seedUser(role store.UserRole) store.User {
	e.t.Helper()
	u, err := e.q.CreateUser(context.Background(), store.CreateUserParams{
		Email:        uniqueEmail(),
		Name:         uniqueName("Seed User"),
		Role:         role,
		PasswordHash: pgtype.Text{String: seedPasswordHash, Valid: true},
	})
	if err != nil {
		e.t.Fatalf("seed user: %v", err)
	}
	return u
}

func (e *env) seedTeam() store.Team {
	e.t.Helper()
	tm, err := e.q.CreateTeam(context.Background(), store.CreateTeamParams{
		Name: uniqueName("team"),
	})
	if err != nil {
		e.t.Fatalf("seed team: %v", err)
	}
	return tm
}

type result struct {
	status  int
	header  http.Header
	body    []byte
	cookies []*http.Cookie
}

// do performs a request against the test server. body (when non-nil) is
// JSON-encoded; cookie (when non-nil) is attached; hdr are extra headers.
func (e *env) do(method, path string, cookie *http.Cookie, body any, hdr ...[2]string) result {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, reader)
	if err != nil {
		e.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for _, h := range hdr {
		req.Header.Set(h[0], h[1])
	}
	res, err := e.srv.Client().Do(req)
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

// login authenticates and returns the sd_session cookie.
func (e *env) login(email, password string) (*http.Cookie, result) {
	e.t.Helper()
	res := e.do(http.MethodPost, "/api/v1/auth/login", nil, map[string]string{
		"email": email, "password": password,
	})
	for _, c := range res.cookies {
		if c.Name == auth.SessionCookieName {
			return c, res
		}
	}
	return nil, res
}

// mustLogin fails the test unless login succeeds with a session cookie.
func (e *env) mustLogin(email, password string) *http.Cookie {
	e.t.Helper()
	c, res := e.login(email, password)
	if res.status != http.StatusNoContent || c == nil {
		e.t.Fatalf("login %s: status %d (want 204 with cookie); body %s", email, res.status, res.body)
	}
	return c
}

func (e *env) decode(res result, dest any) {
	e.t.Helper()
	if err := json.Unmarshal(res.body, dest); err != nil {
		e.t.Fatalf("decode response %s: %v", res.body, err)
	}
}

// TestAuthMatrix exercises every key endpoint as anon, customer, agent, and
// admin, asserting the exact status per principal and problem+json on
// rejections. Deny-by-default: below admin, only /auth/* plus agent READS
// of users/teams (the workspace's assignee picker, team routing, and
// requester lookups) are reachable; all mutations stay admin-only.
func TestAuthMatrix(t *testing.T) {
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

	// Fixtures for mutating endpoints (only the admin row ever mutates).
	victim := e.seedUser(store.UserRoleCustomer)
	patchTarget := e.seedUser(store.UserRoleCustomer)
	team := e.seedTeam()
	doomedTeam := e.seedTeam()

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
		{"get me", http.MethodGet, "/api/v1/auth/me", nil,
			expect(401, 200, 200, 200)},
		{"list users", http.MethodGet, "/api/v1/users", nil,
			expect(401, 403, 200, 200)},
		{"create user", http.MethodPost, "/api/v1/users",
			map[string]any{"email": uniqueEmail(), "name": "Matrix Made", "role": "customer"},
			expect(401, 403, 403, 201)},
		{"get user", http.MethodGet, "/api/v1/users/" + victim.ID.String(), nil,
			expect(401, 403, 200, 200)},
		{"update user", http.MethodPatch, "/api/v1/users/" + patchTarget.ID.String(),
			map[string]any{"name": "Renamed"},
			expect(401, 403, 403, 200)},
		{"deactivate user", http.MethodDelete, "/api/v1/users/" + victim.ID.String(), nil,
			expect(401, 403, 403, 204)},
		{"list teams", http.MethodGet, "/api/v1/teams", nil,
			expect(401, 403, 200, 200)},
		{"create team", http.MethodPost, "/api/v1/teams",
			map[string]any{"name": uniqueName("matrix-team")},
			expect(401, 403, 403, 201)},
		{"get team", http.MethodGet, "/api/v1/teams/" + team.ID.String(), nil,
			expect(401, 403, 200, 200)},
		{"update team", http.MethodPatch, "/api/v1/teams/" + team.ID.String(),
			map[string]any{"description": "matrix"},
			expect(401, 403, 403, 200)},
		{"set team members", http.MethodPut, "/api/v1/teams/" + team.ID.String() + "/members",
			map[string]any{"user_ids": []string{customerU.ID.String()}},
			expect(401, 403, 403, 200)},
		{"delete team", http.MethodDelete, "/api/v1/teams/" + doomedTeam.ID.String(), nil,
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
				if want == 401 || want == 403 {
					if ct := res.header.Get("Content-Type"); ct != "application/problem+json" {
						t.Fatalf("rejection Content-Type = %q, want application/problem+json", ct)
					}
					var p struct {
						Title  string `json:"title"`
						Status int    `json:"status"`
					}
					e.decode(res, &p)
					if p.Status != want || p.Title == "" {
						t.Fatalf("problem body mismatch: %s", res.body)
					}
				}
			})
		}
	}

	t.Run("unknown api route is problem 404", func(t *testing.T) {
		res := e.do(http.MethodGet, "/api/v1/nope", cookies["admin"], nil)
		if res.status != http.StatusNotFound {
			t.Fatalf("status %d, want 404", res.status)
		}
		if ct := res.header.Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q", ct)
		}
	})

	// Parameter binding failures happen before the generated wrapper runs
	// the middlewares; the auth policy must still gate the response so
	// anonymous callers cannot probe parameter formats on admin routes.
	t.Run("binding error still requires auth", func(t *testing.T) {
		if res := e.do(http.MethodGet, "/api/v1/users/not-a-uuid", nil, nil); res.status != http.StatusUnauthorized {
			t.Fatalf("anon bad uuid: status %d, want 401", res.status)
		}
		if res := e.do(http.MethodGet, "/api/v1/users/not-a-uuid", cookies["customer"], nil); res.status != http.StatusForbidden {
			t.Fatalf("customer bad uuid: status %d, want 403", res.status)
		}
	})
}

func TestLoginValidationAndFailures(t *testing.T) {
	e := newEnv(t)
	u := e.seedUser(store.UserRoleCustomer)

	deactivated := e.seedUser(store.UserRoleCustomer)
	if err := e.q.DeactivateUser(context.Background(), deactivated.ID); err != nil {
		t.Fatalf("deactivate seed: %v", err)
	}
	noPassword, err := e.q.CreateUser(context.Background(), store.CreateUserParams{
		Email: uniqueEmail(), Name: "OIDC Only", Role: store.UserRoleCustomer,
	})
	if err != nil {
		t.Fatalf("seed passwordless user: %v", err)
	}

	tests := []struct {
		name     string
		email    string
		password string
		want     int
	}{
		{"malformed email", "not-an-email", seedPassword, 400},
		{"empty email", "", seedPassword, 400},
		{"empty password", u.Email, "", 400},
		{"unknown email", "nobody-here@example.com", seedPassword, 401},
		{"wrong password", u.Email, "wrong-password-guess", 401},
		{"deactivated account", deactivated.Email, seedPassword, 401},
		{"account without password", noPassword.Email, seedPassword, 401},
		{"correct credentials", u.Email, seedPassword, 204},
		{"email is case-insensitive", strings.ToUpper(u.Email), seedPassword, 204},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, res := e.login(tt.email, tt.password)
			if res.status != tt.want {
				t.Fatalf("login(%q): status %d, want %d; body %s", tt.email, res.status, tt.want, res.body)
			}
			if tt.want == 204 && c == nil {
				t.Fatal("expected session cookie on success")
			}
			if tt.want != 204 && c != nil {
				t.Fatal("must not set a session cookie on failure")
			}
		})
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	u := e.seedUser(store.UserRoleCustomer)

	// Burst is 8 per IP+email: eight failures are 401, the ninth attempt is
	// throttled — even with the correct password.
	for i := 0; i < 8; i++ {
		_, res := e.login(u.Email, "wrong-password-guess")
		if res.status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, res.status)
		}
	}
	_, res := e.login(u.Email, seedPassword)
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt: status %d, want 429; body %s", res.status, res.body)
	}
	if res.header.Get("Retry-After") == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if ct := res.header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("429 Content-Type = %q", ct)
	}

	// Another email from the same IP is unaffected.
	other := e.seedUser(store.UserRoleCustomer)
	e.mustLogin(other.Email, seedPassword)
}

func TestSessionCookie(t *testing.T) {
	e := newEnv(t)
	u := e.seedUser(store.UserRoleCustomer)

	t.Run("flags over http", func(t *testing.T) {
		c := e.mustLogin(u.Email, seedPassword)
		if !c.HttpOnly {
			t.Error("cookie must be HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Errorf("SameSite = %v, want Lax", c.SameSite)
		}
		if c.Secure {
			t.Error("cookie must not be Secure over plain http")
		}
		if c.Path != "/" {
			t.Errorf("Path = %q, want /", c.Path)
		}
		if c.MaxAge <= 0 {
			t.Errorf("MaxAge = %d, want > 0", c.MaxAge)
		}
	})

	t.Run("secure behind https proxy", func(t *testing.T) {
		res := e.do(http.MethodPost, "/api/v1/auth/login", nil,
			map[string]string{"email": u.Email, "password": seedPassword},
			[2]string{"X-Forwarded-Proto", "https"})
		if res.status != http.StatusNoContent {
			t.Fatalf("login: status %d", res.status)
		}
		var found bool
		for _, c := range res.cookies {
			if c.Name == auth.SessionCookieName {
				found = true
				if !c.Secure {
					t.Error("cookie must be Secure when X-Forwarded-Proto is https")
				}
			}
		}
		if !found {
			t.Fatal("no session cookie set")
		}
	})

	t.Run("logout clears cookie and is idempotent", func(t *testing.T) {
		c := e.mustLogin(u.Email, seedPassword)
		res := e.do(http.MethodPost, "/api/v1/auth/logout", c, nil)
		if res.status != http.StatusNoContent {
			t.Fatalf("logout: status %d", res.status)
		}
		var cleared bool
		for _, sc := range res.cookies {
			if sc.Name == auth.SessionCookieName && sc.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Fatalf("logout must expire the session cookie; got %v", res.cookies)
		}
		// Anonymous logout also succeeds — but without a Set-Cookie, so a
		// cookieless cross-site POST cannot force-log-out a victim.
		res = e.do(http.MethodPost, "/api/v1/auth/logout", nil, nil)
		if res.status != http.StatusNoContent {
			t.Fatalf("anonymous logout: status %d", res.status)
		}
		if len(res.cookies) != 0 {
			t.Fatalf("anonymous logout must not set cookies; got %v", res.cookies)
		}
	})
}

func TestSessionRejection(t *testing.T) {
	e := newEnv(t)
	u := e.seedUser(store.UserRoleCustomer)
	good := e.mustLogin(u.Email, seedPassword)

	me := func(c *http.Cookie) int {
		return e.do(http.MethodGet, "/api/v1/auth/me", c, nil).status
	}

	if got := me(good); got != http.StatusOK {
		t.Fatalf("valid session: %d, want 200", got)
	}

	t.Run("tampered token", func(t *testing.T) {
		bad := *good
		bad.Value = good.Value + "x"
		if got := me(&bad); got != http.StatusUnauthorized {
			t.Fatalf("tampered: %d, want 401", got)
		}
	})

	t.Run("garbage token", func(t *testing.T) {
		bad := *good
		bad.Value = "garbage"
		if got := me(&bad); got != http.StatusUnauthorized {
			t.Fatalf("garbage: %d, want 401", got)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		token, err := auth.SignSession(e.secret, auth.Session{
			UserID:       u.ID,
			TokenVersion: u.TokenVersion,
			ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
		})
		if err != nil {
			t.Fatalf("sign expired session: %v", err)
		}
		if got := me(&http.Cookie{Name: auth.SessionCookieName, Value: token}); got != http.StatusUnauthorized {
			t.Fatalf("expired: %d, want 401", got)
		}
	})

	t.Run("token version mismatch after bump", func(t *testing.T) {
		if _, err := e.q.BumpUserTokenVersion(context.Background(), u.ID); err != nil {
			t.Fatalf("bump token_version: %v", err)
		}
		if got := me(good); got != http.StatusUnauthorized {
			t.Fatalf("stale token_version: %d, want 401", got)
		}
		// A fresh login carries the new token_version and works again.
		fresh := e.mustLogin(u.Email, seedPassword)
		if got := me(fresh); got != http.StatusOK {
			t.Fatalf("fresh session after bump: %d, want 200", got)
		}
	})

	t.Run("unknown user id in valid token", func(t *testing.T) {
		token, err := auth.SignSession(e.secret, auth.Session{
			UserID:       uuid.New(),
			TokenVersion: 1,
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		})
		if err != nil {
			t.Fatalf("sign session: %v", err)
		}
		if got := me(&http.Cookie{Name: auth.SessionCookieName, Value: token}); got != http.StatusUnauthorized {
			t.Fatalf("unknown user: %d, want 401", got)
		}
	})
}

func TestUsersCRUD(t *testing.T) {
	e := newEnv(t)
	adminU := e.seedUser(store.UserRoleAdmin)
	admin := e.mustLogin(adminU.Email, seedPassword)

	t.Run("create lowercases email", func(t *testing.T) {
		mixed := "MiXeD" + uniqueEmail()
		res := e.do(http.MethodPost, "/api/v1/users", admin,
			map[string]any{"email": mixed, "name": "Case Test", "role": "agent"})
		if res.status != http.StatusCreated {
			t.Fatalf("create: %d; body %s", res.status, res.body)
		}
		var created struct {
			Id    string `json:"id"`
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		e.decode(res, &created)
		if created.Email != strings.ToLower(mixed) {
			t.Fatalf("email = %q, want lowercased %q", created.Email, strings.ToLower(mixed))
		}
		if created.Role != "agent" {
			t.Fatalf("role = %q, want agent", created.Role)
		}

		// Same email, different case: conflict.
		res = e.do(http.MethodPost, "/api/v1/users", admin,
			map[string]any{"email": strings.ToUpper(mixed), "name": "Dupe", "role": "customer"})
		if res.status != http.StatusConflict {
			t.Fatalf("duplicate: %d, want 409; body %s", res.status, res.body)
		}
	})

	t.Run("create validation", func(t *testing.T) {
		tests := []struct {
			name string
			body map[string]any
		}{
			{"invalid email", map[string]any{"email": "nope", "name": "X", "role": "customer"}},
			{"invalid role", map[string]any{"email": uniqueEmail(), "name": "X", "role": "root"}},
			{"short password", map[string]any{"email": uniqueEmail(), "name": "X", "role": "customer", "password": "shortpwd9"}},
			{"unknown field", map[string]any{"email": uniqueEmail(), "name": "X", "role": "customer", "surprise": true}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				res := e.do(http.MethodPost, "/api/v1/users", admin, tt.body)
				if res.status != http.StatusBadRequest {
					t.Fatalf("status %d, want 400; body %s", res.status, res.body)
				}
			})
		}
	})

	t.Run("create with password can log in", func(t *testing.T) {
		email := uniqueEmail()
		res := e.do(http.MethodPost, "/api/v1/users", admin,
			map[string]any{"email": email, "name": "Login Test", "role": "customer", "password": "super-secret-pw"})
		if res.status != http.StatusCreated {
			t.Fatalf("create: %d; body %s", res.status, res.body)
		}
		e.mustLogin(email, "super-secret-pw")
	})

	t.Run("patch role company name", func(t *testing.T) {
		u := e.seedUser(store.UserRoleCustomer)
		res := e.do(http.MethodPatch, "/api/v1/users/"+u.ID.String(), admin,
			map[string]any{"role": "agent", "company": "ACME", "name": "Promoted"})
		if res.status != http.StatusOK {
			t.Fatalf("patch: %d; body %s", res.status, res.body)
		}
		var got struct {
			Role    string `json:"role"`
			Company string `json:"company"`
			Name    string `json:"name"`
			Active  bool   `json:"active"`
		}
		e.decode(res, &got)
		if got.Role != "agent" || got.Company != "ACME" || got.Name != "Promoted" || !got.Active {
			t.Fatalf("patched user mismatch: %s", res.body)
		}
	})

	t.Run("email is immutable", func(t *testing.T) {
		u := e.seedUser(store.UserRoleCustomer)
		res := e.do(http.MethodPatch, "/api/v1/users/"+u.ID.String(), admin,
			map[string]any{"email": uniqueEmail()})
		if res.status != http.StatusBadRequest {
			t.Fatalf("patch email: %d, want 400; body %s", res.status, res.body)
		}
	})

	t.Run("password change kills sessions", func(t *testing.T) {
		u := e.seedUser(store.UserRoleCustomer)
		session := e.mustLogin(u.Email, seedPassword)

		res := e.do(http.MethodPatch, "/api/v1/users/"+u.ID.String(), admin,
			map[string]any{"password": "brand-new-password"})
		if res.status != http.StatusOK {
			t.Fatalf("patch password: %d; body %s", res.status, res.body)
		}
		if got := e.do(http.MethodGet, "/api/v1/auth/me", session, nil).status; got != http.StatusUnauthorized {
			t.Fatalf("old session after password change: %d, want 401", got)
		}
		if _, res := e.login(u.Email, seedPassword); res.status != http.StatusUnauthorized {
			t.Fatalf("old password still works: %d", res.status)
		}
		e.mustLogin(u.Email, "brand-new-password")
	})

	t.Run("delete deactivates and kills sessions", func(t *testing.T) {
		u := e.seedUser(store.UserRoleCustomer)
		session := e.mustLogin(u.Email, seedPassword)

		res := e.do(http.MethodDelete, "/api/v1/users/"+u.ID.String(), admin, nil)
		if res.status != http.StatusNoContent {
			t.Fatalf("delete: %d; body %s", res.status, res.body)
		}
		if got := e.do(http.MethodGet, "/api/v1/auth/me", session, nil).status; got != http.StatusUnauthorized {
			t.Fatalf("session after deactivation: %d, want 401", got)
		}
		if _, res := e.login(u.Email, seedPassword); res.status != http.StatusUnauthorized {
			t.Fatalf("deactivated user can log in: %d", res.status)
		}

		// Row is retained (soft delete) and PATCH active=true restores login.
		res = e.do(http.MethodGet, "/api/v1/users/"+u.ID.String(), admin, nil)
		if res.status != http.StatusOK {
			t.Fatalf("get soft-deleted user: %d", res.status)
		}
		var got struct {
			Active bool `json:"active"`
		}
		e.decode(res, &got)
		if got.Active {
			t.Fatal("user should be inactive after DELETE")
		}
		res = e.do(http.MethodPatch, "/api/v1/users/"+u.ID.String(), admin,
			map[string]any{"active": true})
		if res.status != http.StatusOK {
			t.Fatalf("reactivate: %d; body %s", res.status, res.body)
		}
		e.mustLogin(u.Email, seedPassword)
	})

	t.Run("not found and bad ids", func(t *testing.T) {
		missing := uuid.New().String()
		if res := e.do(http.MethodGet, "/api/v1/users/"+missing, admin, nil); res.status != http.StatusNotFound {
			t.Fatalf("get missing: %d, want 404", res.status)
		}
		if res := e.do(http.MethodPatch, "/api/v1/users/"+missing, admin, map[string]any{"name": "x"}); res.status != http.StatusNotFound {
			t.Fatalf("patch missing: %d, want 404", res.status)
		}
		if res := e.do(http.MethodDelete, "/api/v1/users/"+missing, admin, nil); res.status != http.StatusNotFound {
			t.Fatalf("delete missing: %d, want 404", res.status)
		}
		if res := e.do(http.MethodGet, "/api/v1/users/not-a-uuid", admin, nil); res.status != http.StatusBadRequest {
			t.Fatalf("bad uuid: %d, want 400", res.status)
		}
	})

	t.Run("list filters", func(t *testing.T) {
		needle := fmt.Sprintf("needle%d", uniqueCounter.Add(1))
		created := e.do(http.MethodPost, "/api/v1/users", admin, map[string]any{
			"email": needle + "@example.com", "name": "Haystack Person", "role": "customer",
		})
		if created.status != http.StatusCreated {
			t.Fatalf("create: %d", created.status)
		}

		list := func(query string) []struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} {
			res := e.do(http.MethodGet, "/api/v1/users"+query, admin, nil)
			if res.status != http.StatusOK {
				t.Fatalf("list %q: %d; body %s", query, res.status, res.body)
			}
			var users []struct {
				Email string `json:"email"`
				Role  string `json:"role"`
			}
			e.decode(res, &users)
			return users
		}

		if got := list("?q=" + needle); len(got) != 1 || got[0].Email != needle+"@example.com" {
			t.Fatalf("q filter: got %v", got)
		}
		// Substring match against name too.
		if got := list("?q=Haystack+Person"); len(got) == 0 {
			t.Fatal("q filter should match name")
		}
		// ILIKE wildcards in q are literals, not patterns.
		if got := list("?q=%25"); len(got) != 0 {
			t.Fatalf("wildcard should be literal; got %d users", len(got))
		}
		for _, u := range list("?role=admin") {
			if u.Role != "admin" {
				t.Fatalf("role filter leaked %v", u)
			}
		}
		if got := list("?limit=1"); len(got) != 1 {
			t.Fatalf("limit=1: got %d users", len(got))
		}

		for _, bad := range []string{"?limit=0", "?limit=201", "?offset=-1", "?role=bogus", "?active=maybe"} {
			if res := e.do(http.MethodGet, "/api/v1/users"+bad, admin, nil); res.status != http.StatusBadRequest {
				t.Fatalf("list %q: %d, want 400", bad, res.status)
			}
		}
	})
}

func TestTeamsCRUD(t *testing.T) {
	e := newEnv(t)
	adminU := e.seedUser(store.UserRoleAdmin)
	admin := e.mustLogin(adminU.Email, seedPassword)

	type teamResp struct {
		Id          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Members     []struct {
			Id string `json:"id"`
		} `json:"members"`
	}

	t.Run("create get update delete", func(t *testing.T) {
		name := uniqueName("support")
		res := e.do(http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": name})
		if res.status != http.StatusCreated {
			t.Fatalf("create: %d; body %s", res.status, res.body)
		}
		var created teamResp
		e.decode(res, &created)
		if created.Name != name || created.Description != "" {
			t.Fatalf("created team mismatch: %s", res.body)
		}

		if res := e.do(http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": name}); res.status != http.StatusConflict {
			t.Fatalf("duplicate name: %d, want 409", res.status)
		}
		if res := e.do(http.MethodPost, "/api/v1/teams", admin, map[string]any{"name": ""}); res.status != http.StatusBadRequest {
			t.Fatalf("empty name: %d, want 400", res.status)
		}

		res = e.do(http.MethodPatch, "/api/v1/teams/"+created.Id, admin,
			map[string]any{"description": "front line"})
		if res.status != http.StatusOK {
			t.Fatalf("patch: %d; body %s", res.status, res.body)
		}
		var patched teamResp
		e.decode(res, &patched)
		if patched.Description != "front line" || patched.Name != name {
			t.Fatalf("patch result mismatch: %s", res.body)
		}

		// Renaming to another team's name conflicts.
		other := e.seedTeam()
		if res := e.do(http.MethodPatch, "/api/v1/teams/"+created.Id, admin,
			map[string]any{"name": other.Name}); res.status != http.StatusConflict {
			t.Fatalf("rename conflict: %d, want 409", res.status)
		}

		if res := e.do(http.MethodDelete, "/api/v1/teams/"+created.Id, admin, nil); res.status != http.StatusNoContent {
			t.Fatalf("delete: %d", res.status)
		}
		if res := e.do(http.MethodGet, "/api/v1/teams/"+created.Id, admin, nil); res.status != http.StatusNotFound {
			t.Fatalf("get after delete: %d, want 404", res.status)
		}
	})

	t.Run("membership replace", func(t *testing.T) {
		team := e.seedTeam()
		a := e.seedUser(store.UserRoleAgent)
		b := e.seedUser(store.UserRoleAgent)

		put := func(ids []string) result {
			return e.do(http.MethodPut, "/api/v1/teams/"+team.ID.String()+"/members", admin,
				map[string]any{"user_ids": ids})
		}
		memberIDs := func(res result) []string {
			var tr teamResp
			e.decode(res, &tr)
			ids := make([]string, 0, len(tr.Members))
			for _, m := range tr.Members {
				ids = append(ids, m.Id)
			}
			return ids
		}

		res := put([]string{a.ID.String(), b.ID.String()})
		if res.status != http.StatusOK {
			t.Fatalf("put members: %d; body %s", res.status, res.body)
		}
		if got := memberIDs(res); len(got) != 2 {
			t.Fatalf("members = %v, want both", got)
		}

		// Unknown id fails the whole request; membership is unchanged.
		ghost := uuid.New().String()
		res = put([]string{a.ID.String(), ghost})
		if res.status != http.StatusBadRequest {
			t.Fatalf("unknown member: %d, want 400; body %s", res.status, res.body)
		}
		if !strings.Contains(string(res.body), ghost) {
			t.Fatalf("400 should name the unknown id; body %s", res.body)
		}
		check := e.do(http.MethodGet, "/api/v1/teams/"+team.ID.String(), admin, nil)
		if got := memberIDs(check); len(got) != 2 {
			t.Fatalf("membership changed by failed PUT: %v", got)
		}

		// Idempotent replacement shrinks and empties.
		if got := memberIDs(put([]string{b.ID.String()})); len(got) != 1 || got[0] != b.ID.String() {
			t.Fatalf("replace with one: %v", got)
		}
		if got := memberIDs(put([]string{})); len(got) != 0 {
			t.Fatalf("replace with empty: %v", got)
		}
		// user_ids: null is not an empty list; the field is required.
		res = e.do(http.MethodPut, "/api/v1/teams/"+team.ID.String()+"/members", admin,
			map[string]any{"user_ids": nil})
		if res.status != http.StatusBadRequest {
			t.Fatalf("null user_ids: %d, want 400", res.status)
		}
	})

	t.Run("not found", func(t *testing.T) {
		missing := uuid.New().String()
		if res := e.do(http.MethodGet, "/api/v1/teams/"+missing, admin, nil); res.status != http.StatusNotFound {
			t.Fatalf("get: %d", res.status)
		}
		if res := e.do(http.MethodPatch, "/api/v1/teams/"+missing, admin, map[string]any{"name": uniqueName("x")}); res.status != http.StatusNotFound {
			t.Fatalf("patch: %d", res.status)
		}
		if res := e.do(http.MethodDelete, "/api/v1/teams/"+missing, admin, nil); res.status != http.StatusNotFound {
			t.Fatalf("delete: %d", res.status)
		}
		if res := e.do(http.MethodPut, "/api/v1/teams/"+missing+"/members", admin, map[string]any{"user_ids": []string{}}); res.status != http.StatusNotFound {
			t.Fatalf("put members: %d", res.status)
		}
	})
}

func TestGetCurrentUserBody(t *testing.T) {
	e := newEnv(t)
	u := e.seedUser(store.UserRoleAgent)
	c := e.mustLogin(u.Email, seedPassword)

	res := e.do(http.MethodGet, "/api/v1/auth/me", c, nil)
	if res.status != http.StatusOK {
		t.Fatalf("me: %d", res.status)
	}
	var got map[string]any
	e.decode(res, &got)
	if got["email"] != u.Email || got["role"] != "agent" || got["id"] != u.ID.String() {
		t.Fatalf("me body mismatch: %s", res.body)
	}
	// Sensitive columns must never serialize.
	for _, forbidden := range []string{"password_hash", "token_version", "oidc_subject", "oidc_issuer"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("me body leaks %q: %s", forbidden, res.body)
		}
	}
}
