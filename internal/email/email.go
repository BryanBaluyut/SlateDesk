// Package email is the M3 email engine (architecture doc §4):
//
//   - auth.go: per-mailbox SASL credentials — basic (PLAIN), XOAUTH2 for
//     M365 client-credentials and Google refresh-token flows, with the
//     access token cached (encrypted) in mailboxes.credentials_enc and
//     refreshed on expiry.
//   - supervisor.go: one supervised goroutine per active mailbox holding
//     the IMAP IDLE connection; ownership via short DB leases so exactly
//     one worker process polls each mailbox at any replica count.
//   - inbound.go: ProcessMessage — dedup on Message-ID, loop guard,
//     RFC-correct threading, MIME/attachment extraction, everything
//     committed in ONE transaction, IMAP \Seen set only after commit.
//   - outbound.go: BuildAndSend — outbound Message-ID recorded BEFORE the
//     SMTP send, In-Reply-To/References chains, subject token, delivery
//     status badges.
//   - jobs.go: the River workers (email_send, auto-ack, notify-assignee)
//     and the ticket.Mailer implementation that lets the ticket service
//     enqueue outbound email atomically with its own writes.
package email

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/microcosm-cc/bluemonday"
	"github.com/riverqueue/river"

	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// maxInboundMessageBytes caps the raw size of an inbound message the
// supervisor will fetch (checked against RFC822.SIZE before any body bytes
// leave the server). enmime decodes every MIME part into memory, so THIS
// bound — not maxAttachmentBytes — is what actually limits per-message
// memory; it is sized so a message carrying maxAttachmentBytes of base64-
// encoded attachments (~4/3 overhead) still fits. Oversized messages are
// ingested as a header-only notice article (threaded, deduped, never
// silently dropped) via Engine.ProcessOversized.
const maxInboundMessageBytes int64 = 75 << 20 // 75 MiB

// maxAttachmentBytes caps the total decoded attachment payload PERSISTED
// per inbound message (parts beyond the cap are skipped with a visible
// system note, the mail itself still ingests). Peak decode memory is
// bounded by maxInboundMessageBytes above, not by this.
const maxAttachmentBytes int64 = 50 << 20 // 50 MiB

// Engine bundles the email pipeline's dependencies. One Engine serves a
// whole process; it is safe for concurrent use once SetRiver has been
// called (before workers/supervisor start).
type Engine struct {
	pool  *pgxpool.Pool
	q     *store.Queries
	auth  *Auth
	blobs storage.Storage
	html  *bluemonday.Policy

	// river enqueues follow-up jobs inside pipeline transactions. Set via
	// SetRiver after the jobs client exists (the client itself needs the
	// workers registered first, hence the two-step wiring). A nil river
	// disables enqueues — inbound mail still ingests.
	river *river.Client[pgx.Tx]

	// now is swappable in tests.
	now func() time.Time
}

// NewEngine returns an Engine. auth carries the credential/secrets
// handling; blobs stores attachment bytes.
func NewEngine(pool *pgxpool.Pool, auth *Auth, blobs storage.Storage) *Engine {
	return &Engine{
		pool:  pool,
		q:     store.New(pool),
		auth:  auth,
		blobs: blobs,
		html:  bluemonday.UGCPolicy(),
		now:   time.Now,
	}
}

// SetRiver wires the River client used for in-transaction job enqueues.
// Call once during process wiring, before workers or the supervisor start.
func (e *Engine) SetRiver(rc *river.Client[pgx.Tx]) { e.river = rc }

// Auth exposes the engine's credential/token handling to the HTTP layer
// (mailbox test connections, the Google connect flow).
func (e *Engine) Auth() *Auth { return e.auth }

// addTicketEvent inserts a ticket_events audit row (same shape as the
// ticket service's addEvent; duplicated here because that one is
// unexported and tied to the Service receiver).
func addTicketEvent(ctx context.Context, q *store.Queries, ticketID uuid.UUID, actorID *uuid.UUID, typ string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	actor := pgtype.UUID{}
	if actorID != nil {
		actor = pgtype.UUID{Bytes: *actorID, Valid: true}
	}
	if _, err := q.CreateTicketEvent(ctx, store.CreateTicketEventParams{
		TicketID: ticketID,
		ActorID:  actor,
		Type:     typ,
		Payload:  raw,
	}); err != nil {
		return fmt.Errorf("insert %s event: %w", typ, err)
	}
	return nil
}

// notifyTx fires pg_notify inside tx — Postgres delivers only on commit,
// so a rolled-back ingest can never emit a phantom SSE event.
func notifyTx(ctx context.Context, tx pgx.Tx, typ string, ticketID uuid.UUID) error {
	payload, err := json.Marshal(map[string]string{
		"type":      typ,
		"ticket_id": ticketID.String(),
	})
	if err != nil {
		return fmt.Errorf("marshal %s notification: %w", typ, err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", events.Channel, string(payload)); err != nil {
		return fmt.Errorf("pg_notify %s: %w", typ, err)
	}
	return nil
}

// inTx runs fn inside one transaction, committing on nil (same shape as
// the ticket service — every pipeline write is all-or-nothing).
func (e *Engine) inTx(ctx context.Context, fn func(q *store.Queries, tx pgx.Tx) error) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := fn(store.New(tx), tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
