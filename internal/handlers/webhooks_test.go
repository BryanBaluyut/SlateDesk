package handlers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/BryanBaluyut/slatedesk/internal/jobs"
	"github.com/BryanBaluyut/slatedesk/internal/secrets"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
	"github.com/BryanBaluyut/slatedesk/internal/webhook"
)

// webhookBox derives the same signing-secret box handlers.New uses, so tests
// can encrypt secrets they store and decrypt what handlers wrote.
func (e *env) webhookBox() *secrets.Box {
	e.t.Helper()
	box, err := secrets.NewBox(e.secret, secrets.PurposeWebhookSecret)
	if err != nil {
		e.t.Fatalf("webhook box: %v", err)
	}
	return box
}

// seedWebhook inserts an endpoint row directly with a known plaintext secret.
func (e *env) seedWebhook(secret, url string, active bool, events ...string) store.Webhook {
	e.t.Helper()
	enc, err := e.webhookBox().Encrypt([]byte(secret))
	if err != nil {
		e.t.Fatalf("encrypt webhook secret: %v", err)
	}
	wh, err := e.q.CreateWebhook(context.Background(), store.CreateWebhookParams{
		Name:      uniqueName("hook"),
		Url:       url,
		SecretEnc: enc,
		Events:    events,
		Active:    active,
	})
	if err != nil {
		e.t.Fatalf("seed webhook: %v", err)
	}
	return wh
}

// controllableSink is an httptest endpoint whose status is switchable and that
// records the last signature + body it received.
type controllableSink struct {
	mu      sync.Mutex
	status  int
	sig     string
	body    []byte
	hits    int
}

func newSink(status int) (*controllableSink, *httptest.Server) {
	s := &controllableSink{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.hits++
		s.sig = r.Header.Get(webhook.SignatureHeader)
		s.body = body
		w.WriteHeader(s.status)
	}))
	return s, srv
}

func (s *controllableSink) setStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = code
}

func (s *controllableSink) snapshot() (sig string, body []byte, hits int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sig, append([]byte(nil), s.body...), s.hits
}

// TestWebhookCRUDAuthAndSecretEgress: admin-only CRUD; the signing secret is
// returned exactly once at creation and never in any read path.
func TestWebhookCRUDAuthAndSecretEgress(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	agent := e.seedUser(store.UserRoleAgent)
	adminCookie := e.mustLogin(admin.Email, seedPassword)

	const plaintextSecret = "super-secret-signing-value-123"
	create := e.do(http.MethodPost, "/api/v1/webhooks", adminCookie, map[string]any{
		"name":   "orders sink",
		"url":    "https://example.test/hook",
		"events": []string{"ticket.created", "article.created"},
		"secret": plaintextSecret,
	})
	if create.status != http.StatusCreated {
		t.Fatalf("create webhook = %d, want 201; body %s", create.status, create.body)
	}
	var created struct {
		Id            string `json:"id"`
		SigningSecret string `json:"signing_secret"`
	}
	e.decode(create, &created)
	if created.SigningSecret != plaintextSecret {
		t.Fatalf("create echoed signing_secret %q, want %q", created.SigningSecret, plaintextSecret)
	}

	// Stored secret is encrypted at rest and round-trips to the plaintext.
	row, err := e.q.GetWebhook(context.Background(), uuid.MustParse(created.Id))
	if err != nil {
		t.Fatalf("load stored webhook: %v", err)
	}
	if row.SecretEnc == plaintextSecret || !strings.HasPrefix(row.SecretEnc, "v1:") {
		t.Fatalf("secret not encrypted at rest: %q", row.SecretEnc)
	}
	dec, err := e.webhookBox().Decrypt(row.SecretEnc)
	if err != nil || string(dec) != plaintextSecret {
		t.Fatalf("stored secret did not round-trip: %v / %q", err, dec)
	}

	// No read path re-exposes the secret.
	list := e.do(http.MethodGet, "/api/v1/webhooks", adminCookie, nil)
	if list.status != http.StatusOK || strings.Contains(string(list.body), plaintextSecret) {
		t.Fatalf("list leaked secret or bad status %d: %s", list.status, list.body)
	}
	var rows []map[string]any
	e.decode(list, &rows)
	for _, wh := range rows {
		for _, banned := range []string{"secret", "secret_enc", "signing_secret"} {
			if _, ok := wh[banned]; ok {
				t.Errorf("list element exposes %q", banned)
			}
		}
	}

	// Rotate the secret via PATCH: response must NOT echo the new secret.
	patch := e.do(http.MethodPatch, "/api/v1/webhooks/"+created.Id, adminCookie, map[string]any{
		"secret": "rotated-secret-value",
		"active": false,
	})
	if patch.status != http.StatusOK {
		t.Fatalf("patch webhook = %d; body %s", patch.status, patch.body)
	}
	if strings.Contains(string(patch.body), "rotated-secret-value") {
		t.Fatalf("patch response leaked the rotated secret")
	}

	// Auth: non-admins are refused across the surface.
	agentCookie := e.mustLogin(agent.Email, seedPassword)
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/webhooks"},
		{http.MethodPost, "/api/v1/webhooks"},
		{http.MethodPatch, "/api/v1/webhooks/" + created.Id},
		{http.MethodDelete, "/api/v1/webhooks/" + created.Id},
		{http.MethodPost, "/api/v1/webhooks/" + created.Id + "/test"},
		{http.MethodGet, "/api/v1/webhooks/" + created.Id + "/deliveries"},
	} {
		if res := e.do(tc.method, tc.path, agentCookie, map[string]any{}); res.status != http.StatusForbidden {
			t.Errorf("agent %s %s = %d, want 403", tc.method, tc.path, res.status)
		}
		if res := e.do(tc.method, tc.path, nil, map[string]any{}); res.status != http.StatusUnauthorized {
			t.Errorf("anon %s %s = %d, want 401", tc.method, tc.path, res.status)
		}
	}

	// Delete cascades; second delete is 404.
	if res := e.do(http.MethodDelete, "/api/v1/webhooks/"+created.Id, adminCookie, nil); res.status != http.StatusNoContent {
		t.Fatalf("delete webhook = %d", res.status)
	}
	if res := e.do(http.MethodDelete, "/api/v1/webhooks/"+created.Id, adminCookie, nil); res.status != http.StatusNotFound {
		t.Errorf("re-delete = %d, want 404", res.status)
	}
}

// TestWebhookTestPing exercises POST /webhooks/{id}/test against a live sink:
// a 2xx is ok=true; a non-2xx is HTTP 200 with ok=false; the sink receives a
// valid signature.
func TestWebhookTestPing(t *testing.T) {
	e := newEnv(t)
	admin := e.seedUser(store.UserRoleAdmin)
	cookie := e.mustLogin(admin.Email, seedPassword)

	const secret = "ping-secret"
	sink, srv := newSink(http.StatusOK)
	defer srv.Close()
	wh := e.seedWebhook(secret, srv.URL, true, "ticket.created")

	ok := e.do(http.MethodPost, "/api/v1/webhooks/"+wh.ID.String()+"/test", cookie, nil)
	if ok.status != http.StatusOK {
		t.Fatalf("test ping = %d; body %s", ok.status, ok.body)
	}
	var okRes struct {
		Ok           bool `json:"ok"`
		ResponseCode *int `json:"response_code"`
	}
	e.decode(ok, &okRes)
	if !okRes.Ok || okRes.ResponseCode == nil || *okRes.ResponseCode != 200 {
		t.Fatalf("ping result = %+v, want ok=true code=200", okRes)
	}
	// The sink got a signature that verifies against the body it received.
	sig, body, hits := sink.snapshot()
	if hits == 0 {
		t.Fatal("sink never received the ping")
	}
	if want := webhook.SignBody([]byte(secret), body); sig != want {
		t.Errorf("ping signature %q does not verify against body (want %q)", sig, want)
	}

	// A failing endpoint is still HTTP 200 with ok=false.
	sink.setStatus(http.StatusInternalServerError)
	bad := e.do(http.MethodPost, "/api/v1/webhooks/"+wh.ID.String()+"/test", cookie, nil)
	if bad.status != http.StatusOK {
		t.Fatalf("failing ping wrapper status = %d, want 200", bad.status)
	}
	var badRes struct {
		Ok           bool `json:"ok"`
		ResponseCode *int `json:"response_code"`
	}
	e.decode(bad, &badRes)
	if badRes.Ok || badRes.ResponseCode == nil || *badRes.ResponseCode != 500 {
		t.Fatalf("failing ping result = %+v, want ok=false code=500", badRes)
	}
}

// TestWebhookWorkerDeliveryAndRetry drives the delivery worker directly: a 500
// records a pending retry (attempts bumped, error stored) and returns an error
// so River retries; a later 200 marks success; a final attempt marks failed.
func TestWebhookWorkerDeliveryAndRetry(t *testing.T) {
	e := newEnv(t)
	box := e.webhookBox()
	worker := webhook.NewWorker(store.New(testPool), box)

	const secret = "worker-secret"
	sink, srv := newSink(http.StatusInternalServerError)
	defer srv.Close()
	wh := e.seedWebhook(secret, srv.URL, true, "ticket.created")

	payload := []byte(`{"event":"ticket.created","ticket_number":"20260718-0007"}`)
	del, err := e.q.InsertWebhookDelivery(context.Background(), store.InsertWebhookDeliveryParams{
		WebhookID: wh.ID,
		EventType: "ticket.created",
		TicketID:  pgtype.UUID{},
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("insert delivery: %v", err)
	}

	// Attempt 1 of 6 against a 500 → retry recorded, error returned.
	job := &river.Job[webhook.DeliveryArgs]{
		JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: 6},
		Args:   webhook.DeliveryArgs{DeliveryID: del.ID},
	}
	if err := worker.Work(context.Background(), job); err == nil {
		t.Fatal("Work on a 500 returned nil; want an error so River retries")
	}
	got := e.getDelivery(t, del.ID)
	if got.Status != store.WebhookDeliveryStatusPending {
		t.Errorf("after 500: status = %q, want pending", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("after 500: attempts = %d, want 1", got.Attempts)
	}
	if !got.ResponseCode.Valid || got.ResponseCode.Int32 != 500 {
		t.Errorf("after 500: response_code = %+v, want 500", got.ResponseCode)
	}
	if !got.LastError.Valid || got.LastError.String == "" {
		t.Errorf("after 500: last_error not recorded")
	}
	// The sink verified a correct signature over the exact stored payload.
	if sig, body, _ := sink.snapshot(); webhook.SignBody([]byte(secret), body) != sig {
		t.Errorf("worker signature does not verify against delivered body")
	}

	// Endpoint recovers → attempt 2 succeeds.
	sink.setStatus(http.StatusOK)
	job.JobRow.Attempt = 2
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("Work on a recovered endpoint: %v", err)
	}
	got = e.getDelivery(t, del.ID)
	if got.Status != store.WebhookDeliveryStatusSuccess {
		t.Errorf("after 200: status = %q, want success", got.Status)
	}
	if got.Attempts != 2 {
		t.Errorf("after 200: attempts = %d, want 2", got.Attempts)
	}
	if !got.DeliveredAt.Valid {
		t.Errorf("after 200: delivered_at not set")
	}
	if got.LastError.Valid {
		t.Errorf("after 200: last_error should be cleared")
	}

	// A fresh delivery that fails on its FINAL attempt is marked failed.
	sink.setStatus(http.StatusInternalServerError)
	del2, err := e.q.InsertWebhookDelivery(context.Background(), store.InsertWebhookDeliveryParams{
		WebhookID: wh.ID, EventType: "ticket.created", TicketID: pgtype.UUID{}, Payload: payload,
	})
	if err != nil {
		t.Fatalf("insert delivery 2: %v", err)
	}
	finalJob := &river.Job[webhook.DeliveryArgs]{
		JobRow: &rivertype.JobRow{Attempt: 6, MaxAttempts: 6},
		Args:   webhook.DeliveryArgs{DeliveryID: del2.ID},
	}
	if err := worker.Work(context.Background(), finalJob); err != nil {
		t.Fatalf("final attempt should not return an error (job is terminal): %v", err)
	}
	got2 := e.getDelivery(t, del2.ID)
	if got2.Status != store.WebhookDeliveryStatusFailed {
		t.Errorf("final failure: status = %q, want failed", got2.Status)
	}
}

// TestWebhookTransactionalEnqueue proves deliveries are enqueued off the
// ticket event, only for active endpoints subscribed to that event type.
func TestWebhookTransactionalEnqueue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	requester := e.seedUser(store.UserRoleCustomer)

	subscribed := e.seedWebhook("s1", "https://example.test/a", true, "ticket.created")
	otherEvent := e.seedWebhook("s2", "https://example.test/b", true, "article.created")
	inactive := e.seedWebhook("s3", "https://example.test/c", false, "ticket.created")

	// A ticket service wired with the webhook dispatcher (as in production).
	jc, err := jobs.NewClient(testPool, nil)
	if err != nil {
		t.Fatalf("jobs client: %v", err)
	}
	svc := ticket.NewService(testPool)
	svc.SetEventSink(webhook.NewDispatcher(jc.River()))

	tkt, _, err := svc.CreateTicket(ctx, ticket.CreateTicketParams{
		Subject:     "please help",
		RequesterID: requester.ID,
		Article: ticket.ArticleInput{
			SenderType: store.ArticleSenderCustomer,
			Channel:    store.ArticleChannelWeb,
			BodyText:   "my thing is broken",
		},
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	// The subscribed, active endpoint got exactly one pending delivery whose
	// payload references the new ticket.
	subDeliveries := e.recentDeliveries(t, subscribed.ID)
	if len(subDeliveries) != 1 {
		t.Fatalf("subscribed webhook deliveries = %d, want 1", len(subDeliveries))
	}
	d := subDeliveries[0]
	if d.EventType != ticket.NotifyTicketCreated {
		t.Errorf("delivery event = %q, want ticket.created", d.EventType)
	}
	if !d.TicketID.Valid || uuid.UUID(d.TicketID.Bytes) != tkt.ID {
		t.Errorf("delivery ticket_id mismatch")
	}
	if !strings.Contains(string(d.Payload), tkt.Number) {
		t.Errorf("payload %s does not carry ticket number %s", d.Payload, tkt.Number)
	}

	// The endpoint on a different event and the inactive endpoint got nothing.
	if got := e.recentDeliveries(t, otherEvent.ID); len(got) != 0 {
		t.Errorf("non-subscribed webhook got %d deliveries, want 0", len(got))
	}
	if got := e.recentDeliveries(t, inactive.ID); len(got) != 0 {
		t.Errorf("inactive webhook got %d deliveries, want 0", len(got))
	}
}

func (e *env) getDelivery(t *testing.T, id int64) store.WebhookDelivery {
	t.Helper()
	d, err := e.q.GetWebhookDelivery(context.Background(), id)
	if err != nil {
		t.Fatalf("get delivery %d: %v", id, err)
	}
	return d
}

func (e *env) recentDeliveries(t *testing.T, webhookID uuid.UUID) []store.WebhookDelivery {
	t.Helper()
	rows, err := e.q.ListRecentWebhookDeliveries(context.Background(), store.ListRecentWebhookDeliveriesParams{
		WebhookID: webhookID,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	return rows
}
