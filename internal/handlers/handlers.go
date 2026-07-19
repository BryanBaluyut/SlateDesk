// Package handlers implements the generated api.ServerInterface: auth
// (login/logout/me), users CRUD, teams CRUD + membership, and the M2 ticket
// core (tickets, articles, attachments, tags, dashboard counters, SSE
// events). All errors are RFC 9457 problem+json; authorization is enforced
// by the RequireUser / RequireRole middleware wired in Router.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/email"
	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/secrets"
	"github.com/BryanBaluyut/slatedesk/internal/settings"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
	"github.com/BryanBaluyut/slatedesk/internal/webhook"
)

// Kicker pokes the in-process mailbox supervisor for an immediate
// reconcile after a mailbox mutation (create/update/delete). Satisfied by
// *email.Supervisor; nil when the supervisor runs in another process
// (`serve --no-worker`), where the reconcile interval picks changes up.
type Kicker interface {
	Kick()
}

// Login rate limits. Per IP+email: loginBurst immediate attempts, then one
// more every loginRefill — the anti-guessing brake for a single account.
// Per IP: an aggregate cap across all emails, so one address cannot force
// unbounded argon2id work (each verify pins 64 MiB) by enumerating fresh
// email values; generous enough for an office NAT.
const (
	loginBurst  = 8
	loginRefill = 30 * time.Second

	loginIPBurst  = 30
	loginIPRefill = time.Second
)

// Handlers implements api.ServerInterface. The M1-M3 operations are defined
// as explicit methods (they shadow the embedded stubs); the embedded
// api.Unimplemented supplies http.StatusNotImplemented handlers for the M4
// operations whose contract exists in api/openapi.yaml but whose handlers are
// not yet wired, so the compiler-enforced interface assertion below stays
// satisfied and the build stays green.
type Handlers struct {
	// Unimplemented is embedded so newly specified operations compile as 501
	// stubs until an explicit method overrides them.
	api.Unimplemented

	pool         *pgxpool.Pool
	q            *store.Queries
	svc          *ticket.Service   // transactional ticket-core writes
	hub          *events.Hub       // in-process SSE fan-out
	sseSlots     chan struct{}     // bounds concurrent SSE streams (see events.go)
	blobs        storage.Storage   // attachment blob storage
	secret       []byte            // instance secret; signs session cookies + OAuth state
	limiter      *auth.RateLimiter // per IP+email
	ipLimiter    *auth.RateLimiter // per IP, all emails combined
	sessionTTL   time.Duration
	cookieSecure auth.CookieSecureMode

	// M3 email surface.
	engine   *email.Engine   // test connections, Google OAuth, retry-send
	box      *secrets.Box    // mailbox credential encryption at write
	settings *settings.Store // external-url + OAuth state nonces
	kicker   Kicker          // supervisor poke on mailbox mutations (nil-able)

	// M4 surface.
	webhookBox     *secrets.Box      // webhook signing-secret encryption at write
	webhookClient  *http.Client      // synchronous test-ping delivery
	captcha        CaptchaVerifier   // public-form captcha (Noop = off by default)
	publicLimiter  *auth.RateLimiter // per-IP public web-form submissions
	setupCompleted atomic.Bool       // cached once true; gates the API before first-run
}

var _ api.ServerInterface = (*Handlers)(nil)

// New returns Handlers backed by pool, signing sessions with the instance
// secret. cookieSecure controls the session cookie's Secure attribute; hub
// feeds GET /events subscribers (fill it from an events.Listener); blobs
// stores attachment bytes. engine is the M3 email engine: it doubles as
// the ticket service's Mailer (agent replies enqueue their email
// atomically) and backs the mailbox admin surface; kicker (nil-able)
// pokes the in-process mailbox supervisor after mailbox mutations.
func New(pool *pgxpool.Pool, secret []byte, cookieSecure auth.CookieSecureMode, hub *events.Hub, blobs storage.Storage, engine *email.Engine, kicker Kicker) (*Handlers, error) {
	box, err := secrets.NewBox(secret, secrets.PurposeMailboxCredentials)
	if err != nil {
		return nil, fmt.Errorf("handlers: mailbox credentials box: %w", err)
	}
	webhookBox, err := secrets.NewBox(secret, secrets.PurposeWebhookSecret)
	if err != nil {
		return nil, fmt.Errorf("handlers: webhook secret box: %w", err)
	}
	svc := ticket.NewService(pool)
	if engine != nil {
		svc.SetMailer(engine)
		// M4: enqueue durable webhook deliveries transactionally off ticket
		// events, reusing the engine's River client (set during wiring before
		// New is called). nil client (a mis-wired process) leaves the sink
		// unset — webhook dispatch is simply disabled, never a nil deref.
		if rc := engine.River(); rc != nil {
			svc.SetEventSink(webhook.NewDispatcher(rc))
		}
	}
	return &Handlers{
		pool:          pool,
		q:             store.New(pool),
		svc:           svc,
		hub:           hub,
		sseSlots:      make(chan struct{}, maxEventStreams),
		blobs:         blobs,
		secret:        secret,
		limiter:       auth.NewRateLimiter(loginBurst, loginRefill),
		ipLimiter:     auth.NewRateLimiter(loginIPBurst, loginIPRefill),
		sessionTTL:    auth.DefaultSessionTTL,
		cookieSecure:  cookieSecure,
		engine:        engine,
		box:           box,
		settings:      settings.New(pool),
		kicker:        kicker,
		webhookBox:    webhookBox,
		webhookClient: &http.Client{Timeout: webhookTestTimeout},
		captcha:       NoopCaptcha{},
		publicLimiter: auth.NewRateLimiter(publicFormBurst, publicFormRefill),
	}, nil
}

// SetCaptchaVerifier overrides the public-form captcha verifier (default
// NoopCaptcha = off). Call during wiring before the server takes traffic.
func (h *Handlers) SetCaptchaVerifier(v CaptchaVerifier) {
	if v != nil {
		h.captcha = v
	}
}

// kickMailboxes pokes the in-process supervisor (if any) so a mailbox
// mutation takes effect without waiting out the reconcile interval.
func (h *Handlers) kickMailboxes() {
	if h.kicker != nil {
		h.kicker.Kick()
	}
}

// Router returns the API router (mount it at /api/v1). Routing comes from
// the generated spec-first wrapper; the auth policy middleware guards every
// operation (deny-by-default: anything not explicitly public or
// user-accessible requires admin).
func (h *Handlers) Router() http.Handler {
	r := chi.NewRouter()
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, http.StatusNotFound, "Not Found", "no such API route")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, http.StatusMethodNotAllowed, "Method Not Allowed", "")
	})
	return api.HandlerWithOptions(h, api.ChiServerOptions{
		BaseRouter:  r,
		Middlewares: []api.MiddlewareFunc{h.authPolicy},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			// Path/query parameter binding failures from the generated
			// wrapper (e.g. non-UUID id). The wrapper binds parameters
			// before running the middlewares, so route the 400 through the
			// same auth policy: protected routes must answer 401/403 to
			// unauthenticated callers, not leak parameter formats.
			h.authPolicy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				problem.Write(w, r, http.StatusBadRequest, "Bad Request", err.Error())
			})).ServeHTTP(w, r)
		},
	})
}

// writeJSON writes a JSON response body with the given status.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "error", err, "path", r.URL.Path)
	}
}

// decodeJSON decodes the request body into dst, rejecting unknown fields
// and trailing garbage. On failure it writes a 400 problem and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body: trailing data")
		return false
	}
	return true
}

// decodeJSONFields decodes the request body into a field-presence map so
// handlers can distinguish "omitted" from "explicit null" (tri-state PATCH
// semantics). Unknown fields and trailing garbage are rejected. On failure a
// 400 problem is written and ok is false.
func decodeJSONFields(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body: "+err.Error())
		return nil, false
	}
	if dec.More() {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body: trailing data")
		return nil, false
	}
	for key := range fields {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", fmt.Sprintf("invalid JSON body: unknown field %q", key))
			return nil, false
		}
	}
	return fields, true
}

// serverError logs err and writes a generic 500 problem (no internal detail
// leaks to the client).
func serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.Error("handler error", "what", what, "error", err, "method", r.Method, "path", r.URL.Path)
	problem.Write(w, r, http.StatusInternalServerError, "Internal Server Error", "")
}

// isUniqueViolation reports whether err is a Postgres unique constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isNoRows reports whether err means the row was not found.
func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// inTx runs fn inside a transaction with a store bound to it, committing on
// nil and rolling back on error.
func (h *Handlers) inTx(ctx context.Context, fn func(q *store.Queries) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(h.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// toAPIUser converts a store row to its API representation. Sensitive
// columns (password_hash, oidc_*, token_version) never leave here.
func toAPIUser(u store.User) api.User {
	return api.User{
		Id:        u.ID,
		Email:     openapi_types.Email(u.Email),
		Name:      u.Name,
		Role:      api.Role(u.Role),
		Company:   u.Company,
		Active:    u.Active,
		CreatedAt: u.CreatedAt,
	}
}

// toAPIUsers converts a slice, mapping nil to an empty (JSON []) slice.
func toAPIUsers(us []store.User) []api.User {
	out := make([]api.User, 0, len(us))
	for _, u := range us {
		out = append(out, toAPIUser(u))
	}
	return out
}

// toAPITeam converts a store team row to its API representation.
func toAPITeam(t store.Team) api.Team {
	return api.Team{Id: t.ID, Name: t.Name, Description: t.Description}
}

// asString unwraps the interface{}-typed columns sqlc emits for
// COALESCE(citext::text, ...) expressions; pgx scans them as string.
func asString(v any) string {
	s, _ := v.(string)
	return s
}
