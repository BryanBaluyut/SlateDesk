// Package webhook delivers outbound HTTP notifications for ticket/article
// events (M4). It has two halves that share the durable webhook_deliveries
// outbox table:
//
//   - Dispatcher implements ticket.EventSink: inside the ticket service's
//     transaction, for each subscribable event, it inserts a pending
//     webhook_deliveries row AND enqueues a River delivery job on the SAME
//     pgx.Tx. Enqueue is therefore transactional off the event — a delivery
//     can never be scheduled for a rolled-back write, nor lost for a
//     committed one (architecture doc §1: durable outbox off ticket_events).
//   - Worker drains the queue: it loads the delivery row, POSTs the signed
//     payload to the endpoint, and records the outcome. River provides the
//     capped-backoff retries.
//
// Every delivery carries an HMAC-SHA256 signature of the raw body in the
// X-SlateDesk-Signature header, computed with the endpoint's per-webhook
// signing secret (stored AES-GCM-encrypted; secrets.PurposeWebhookSecret),
// plus a stable X-SlateDesk-Delivery id. Because delivery is at-least-once
// (River retries a delivery whose 2xx was lost, re-POSTing a byte-identical
// body), the delivery id is constant across those retries so a receiver can
// dedupe the duplicates.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/BryanBaluyut/slatedesk/internal/jobs"
	"github.com/BryanBaluyut/slatedesk/internal/secrets"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// SignatureHeader carries the hex HMAC-SHA256 of the raw request body,
// formatted "sha256=<hex>".
const SignatureHeader = "X-SlateDesk-Signature"

// DeliveryHeader carries the webhook_deliveries row id, stable across River
// retries of the same delivery, so receivers can dedupe at-least-once
// duplicates (the same event re-POSTed after a lost 2xx).
const DeliveryHeader = "X-SlateDesk-Delivery"

// deliveryMaxAttempts bounds River's capped-backoff retries before a delivery
// goes terminal ('failed').
const deliveryMaxAttempts = 6

// deliverTimeout bounds one HTTP attempt to an endpoint.
const deliverTimeout = 10 * time.Second

// maxErrorLen caps the stored last_error string.
const maxErrorLen = 1000

// subscribableEvents is the set of event types a webhook can subscribe to
// (a subset of the ticket service's notify types; article.updated —
// delivery-badge churn — is deliberately excluded).
var subscribableEvents = map[string]bool{
	ticket.NotifyTicketCreated:  true,
	ticket.NotifyTicketUpdated:  true,
	ticket.NotifyArticleCreated: true,
}

// SignBody returns the value of the X-SlateDesk-Signature header for body
// under secret: "sha256=" + hex(HMAC-SHA256(secret, body)).
func SignBody(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Deliver POSTs body to url, signed with secret and tagged with deliveryID
// (the X-SlateDesk-Delivery dedupe key; 0 omits the header, e.g. a test ping).
// It returns the HTTP status code (0 on transport error) and a non-nil error
// when the attempt did not yield a 2xx (transport failure or non-2xx
// response). The response body is drained and discarded so the connection can
// be reused.
func Deliver(ctx context.Context, client *http.Client, url string, deliveryID int64, secret, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SlateDesk-Webhook/1.0")
	req.Header.Set(SignatureHeader, SignBody(secret, body))
	if deliveryID != 0 {
		req.Header.Set(DeliveryHeader, strconv.FormatInt(deliveryID, 10))
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("deliver: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("endpoint returned status %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// --- Dispatcher (ticket.EventSink) ------------------------------------------

// Dispatcher enqueues durable webhook deliveries transactionally off ticket
// events. Construct with NewDispatcher and wire via ticket.Service.SetEventSink.
type Dispatcher struct {
	river *river.Client[pgx.Tx]
}

var _ ticket.EventSink = (*Dispatcher)(nil)

// NewDispatcher returns a Dispatcher that enqueues on rc.
func NewDispatcher(rc *river.Client[pgx.Tx]) *Dispatcher {
	return &Dispatcher{river: rc}
}

// OnEvent runs inside the ticket service transaction. For a subscribable
// event with at least one active subscriber, it inserts a pending delivery
// row and enqueues a River job for each subscriber — all on tx.
func (d *Dispatcher) OnEvent(ctx context.Context, tx pgx.Tx, eventType string, ticketID uuid.UUID) error {
	if !subscribableEvents[eventType] {
		return nil
	}
	if d.river == nil {
		return errors.New("webhook: river client not wired")
	}
	q := store.New(tx)
	hooks, err := q.ListActiveWebhooksByEvent(ctx, eventType)
	if err != nil {
		return fmt.Errorf("webhook: list subscribers for %s: %w", eventType, err)
	}
	if len(hooks) == 0 {
		return nil
	}

	// One compact, ticket-centric envelope per event (no article/internal
	// bodies leak to third parties — the receiver refetches via the API).
	row, err := q.GetTicket(ctx, ticketID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // ticket vanished mid-transaction; nothing to notify
		}
		return fmt.Errorf("webhook: load ticket %s: %w", ticketID, err)
	}
	payload, err := json.Marshal(map[string]any{
		"event":         eventType,
		"ticket_id":     ticketID.String(),
		"ticket_number": row.Ticket.Number,
		"occurred_at":   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("webhook: marshal %s payload: %w", eventType, err)
	}

	for _, h := range hooks {
		del, err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{
			WebhookID: h.ID,
			EventType: eventType,
			TicketID:  pgtype.UUID{Bytes: ticketID, Valid: true},
			Payload:   payload,
		})
		if err != nil {
			return fmt.Errorf("webhook: enqueue delivery for %s: %w", h.ID, err)
		}
		if _, err := d.river.InsertTx(ctx, tx, DeliveryArgs{DeliveryID: del.ID}, nil); err != nil {
			return fmt.Errorf("webhook: enqueue delivery job for %s: %w", h.ID, err)
		}
	}
	return nil
}

// --- Worker -----------------------------------------------------------------

// DeliveryArgs is the River job: deliver one webhook_deliveries row.
type DeliveryArgs struct {
	DeliveryID int64 `json:"delivery_id"`
}

func (DeliveryArgs) Kind() string { return "webhook_deliver" }

func (DeliveryArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueDefault, MaxAttempts: deliveryMaxAttempts}
}

// Worker delivers queued webhook rows over HTTP. It decrypts each endpoint's
// signing secret with box (secrets.PurposeWebhookSecret).
type Worker struct {
	river.WorkerDefaults[DeliveryArgs]
	q      *store.Queries
	box    *secrets.Box
	client *http.Client
}

// NewWorker builds the delivery worker on pool.
func NewWorker(q *store.Queries, box *secrets.Box) *Worker {
	return &Worker{
		q:      q,
		box:    box,
		client: &http.Client{Timeout: deliverTimeout},
	}
}

// RegisterWorkers registers the webhook delivery worker on reg.
func RegisterWorkers(reg *jobs.Registry, w *Worker) error {
	return jobs.AddWorker(reg, w)
}

// Work delivers one row: load it, POST the signed payload, record the
// outcome. River retries returned errors with capped backoff; the final
// attempt records terminal 'failed'.
func (w *Worker) Work(ctx context.Context, job *river.Job[DeliveryArgs]) error {
	del, err := w.q.GetWebhookDelivery(ctx, job.Args.DeliveryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // webhook (and its cascade of deliveries) deleted
		}
		return fmt.Errorf("webhook: load delivery %d: %w", job.Args.DeliveryID, err)
	}
	if del.Status == store.WebhookDeliveryStatusSuccess {
		return nil // already delivered (redelivery of an acked job)
	}
	hook, err := w.q.GetWebhook(ctx, del.WebhookID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("webhook: load endpoint %s: %w", del.WebhookID, err)
	}
	secret, err := w.box.Decrypt(hook.SecretEnc)
	if err != nil {
		// A secret we cannot decrypt will never succeed; mark failed and stop.
		w.markFailed(ctx, del.ID, 0, "signing secret could not be decrypted")
		return nil
	}

	dctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
	code, deliverErr := Deliver(dctx, w.client, hook.Url, del.ID, secret, del.Payload)
	if deliverErr == nil {
		if err := w.q.MarkWebhookDeliverySuccess(ctx, store.MarkWebhookDeliverySuccessParams{
			ID:           del.ID,
			ResponseCode: pgInt4(code),
		}); err != nil {
			return fmt.Errorf("webhook: mark delivery %d success: %w", del.ID, err)
		}
		return nil
	}

	final := job.Attempt >= job.MaxAttempts
	if final {
		w.markFailed(ctx, del.ID, code, deliverErr.Error())
		slog.Warn("webhook: delivery exhausted retries",
			"delivery_id", del.ID, "webhook_id", del.WebhookID, "error", deliverErr)
		return nil
	}
	if err := w.q.RecordWebhookDeliveryRetry(ctx, store.RecordWebhookDeliveryRetryParams{
		ID:           del.ID,
		ResponseCode: pgInt4(code),
		LastError:    pgText(truncate(deliverErr.Error())),
	}); err != nil {
		return fmt.Errorf("webhook: record delivery %d retry: %w", del.ID, err)
	}
	return deliverErr // ask River to retry with backoff
}

func (w *Worker) markFailed(ctx context.Context, id int64, code int, msg string) {
	if err := w.q.MarkWebhookDeliveryFailed(ctx, store.MarkWebhookDeliveryFailedParams{
		ID:           id,
		ResponseCode: pgInt4(code),
		LastError:    pgText(truncate(msg)),
	}); err != nil {
		slog.Error("webhook: mark delivery failed", "delivery_id", id, "error", err)
	}
}

// pgInt4 maps a status code to a nullable int4 (0 => NULL, meaning transport
// error with no HTTP response).
func pgInt4(code int) pgtype.Int4 {
	if code == 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(code), Valid: true}
}

func pgText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func truncate(s string) string {
	if len(s) > maxErrorLen {
		return s[:maxErrorLen] + "…"
	}
	return s
}
