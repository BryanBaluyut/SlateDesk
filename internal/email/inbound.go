// Inbound pipeline (architecture doc §4, steps adopted verbatim):
//
//  1. dedup on Message-ID (missing id -> content-hash synthesis)
//  2. loop guard (Auto-Submitted / Precedence / X-Auto-Response-Suppress,
//     plus mail whose From is the mailbox's own address — a self-loop)
//  3. threading: References newest-first (walk capped at
//     maxThreadingWalk — the header is attacker-controlled), then
//     In-Reply-To, against email_message_ids in BOTH directions (replies
//     to our own outbound Message-IDs must thread — v1's broken case),
//     then the [SD-…] subject token, else a new ticket
//  4. enmime parse: text/plain preferred (enmime downconverts HTML when
//     absent), bluemonday-sanitized HTML, charset-decoded, attachments
//     (incl. inline parts with a Content-ID) streamed to storage, total
//     capped at maxAttachmentBytes (dropped parts leave an internal
//     system note in the thread, never a silent loss)
//  5. sender lookup by From (citext) else auto-create role=customer
//  6. closed ticket + customer reply -> reopen; loop-guarded (suppressed)
//     mail NEVER flips ticket status — an autoresponder answering a
//     closed ticket must not reopen it
//  7. article + email_message_ids row + ticket_events + job enqueues +
//     pg_notify in ONE transaction
//  8. the caller sets IMAP \Seen only after the transaction commits
package email

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jhillyerd/enmime/v2"

	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// errDuplicateMessage aborts the ingest transaction when the authoritative
// dedup INSERT hits a Message-ID conflict (an IMAP redelivery).
var errDuplicateMessage = errors.New("email: duplicate message")

// ticketTokenRe matches the subject token outbound mail stamps into every
// subject ("[SD-20260718-0001]") — the threading fallback for clients that
// strip References/In-Reply-To.
var ticketTokenRe = regexp.MustCompile(`\[SD-(\d{8}-\d{4})\]`)

// msgIDRe extracts angle-bracketed ids from References/In-Reply-To.
var msgIDRe = regexp.MustCompile(`<([^<>\s]+)>`)

// subjectPrefixRe strips reply/forward chains ("Re: Re: Fwd: …") off the
// front of a subject when it becomes a new ticket's subject. The original
// subject stays visible in the article body/thread.
var subjectPrefixRe = regexp.MustCompile(`(?i)^\s*((re|fw|fwd|aw|wg|sv|antw)(\[\d+\])?\s*:\s*)+`)

// InboundResult reports what ProcessMessage did with a message.
type InboundResult struct {
	// Duplicate: the Message-ID was already ingested; nothing was written.
	// The caller should still mark the message \Seen.
	Duplicate bool
	// Suppressed: the loop guard matched (autoresponder headers, or the
	// mail came From the mailbox's own address). The mail was still
	// ingested (for visibility) but no auto-ack/notify jobs were enqueued
	// and the ticket status was left untouched.
	Suppressed bool
	// NewTicket: no thread matched and a ticket was created.
	NewTicket bool

	TicketID  uuid.UUID
	ArticleID uuid.UUID
}

// ProcessMessage ingests one raw RFC 5322 message received on mb.
//
// It is deliberately infallible for junk input (unparseable From, empty
// bodies, missing Message-ID all degrade gracefully) because a returned
// error leaves the message UNSEEN in the mailbox and it will be retried
// forever. Errors are reserved for infrastructure failures (DB, storage)
// where a retry is what we want.
//
// The IMAP \Seen flag is NOT set here: the caller sets it only after this
// returns nil — at-least-once delivery that the Message-ID dedup makes
// exactly-once (a redelivery after a crash-before-\Seen lands in the
// Duplicate arm and is skipped).
func (e *Engine) ProcessMessage(ctx context.Context, mb store.Mailbox, raw []byte) (InboundResult, error) {
	return e.process(ctx, mb, raw, 0)
}

// ProcessOversized ingests a message whose RFC822.SIZE exceeded
// maxInboundMessageBytes: header holds only the header block
// (BODY.PEEK[HEADER] — the body was never fetched), so dedup, threading,
// loop guard, and sender resolution all still work; the article body is a
// size notice instead of the content. The header bytes are stable across
// redeliveries, so the content-hash Message-ID synthesis fallback keeps
// dedup exact here too.
func (e *Engine) ProcessOversized(ctx context.Context, mb store.Mailbox, header []byte, size int64) (InboundResult, error) {
	return e.process(ctx, mb, header, size)
}

// process is the shared inbound pipeline behind ProcessMessage (oversized
// == 0) and ProcessOversized (oversized = the reported message size; raw
// then holds only the header block).
func (e *Engine) process(ctx context.Context, mb store.Mailbox, raw []byte, oversized int64) (InboundResult, error) {
	var res InboundResult

	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		// Hard MIME failure (enmime tolerates almost anything, so this is
		// rare and structural). Retrying cannot help; ingest a stub so the
		// mail is visible rather than silently looping UNSEEN.
		slog.Warn("email: unparseable message; ingesting stub", "mailbox", mb.EmailAddress, "error", err)
		return e.ingestStub(ctx, mb, raw)
	}

	// Length cap: the header is attacker-controlled and becomes the
	// email_message_ids PRIMARY KEY — a multi-KB id would blow the btree
	// index entry limit (~2.7KB), erroring the ingest transaction on every
	// retry — and is later replayed into outbound References headers,
	// where a single id must stay well under the RFC 5322 998-octet line
	// limit (go-mail folds only at spaces). An over-long id is treated
	// like a missing one: the content-hash synthesis keeps redelivery
	// dedup exact (same bytes, same id) at the cost of the reply-threading
	// anchor — the subject token still threads.
	msgID := pgSafeText(canonicalMessageID(env.GetHeader("Message-Id")))
	if msgID == "" || len(msgID) > maxMessageIDLen {
		msgID = synthesizeMessageID(raw)
	}

	// Fast-path dedup (authoritative re-check happens inside the tx).
	if _, err := e.q.GetEmailMessageID(ctx, msgID); err == nil {
		res.Duplicate = true
		return res, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return res, fmt.Errorf("email: dedup lookup %q: %w", msgID, err)
	}

	res.Suppressed = isAutoResponder(env)

	ticketID, err := e.resolveThread(ctx, env)
	if err != nil {
		return res, err
	}

	fromName, fromAddr := parseFrom(env.GetHeader("From"))
	// Self-loop guard (the loop guard, continued): mail whose From is the
	// mailbox's own address is our own mail reflected back — a
	// misconfigured alias/forwarding rule, or a bounce that kept our From.
	// It ingests for visibility like any loop-guarded mail, but must never
	// trigger auto-ack/notify: the ack would land in this same inbox and
	// be ingested again, forever.
	if strings.EqualFold(fromAddr, mb.EmailAddress) {
		res.Suppressed = true
	}
	subject := pgSafeText(strings.TrimSpace(env.GetHeader("Subject")))

	// pgSafeText everywhere mail-derived text meets the database: a body
	// whose declared charset lies (e.g. Latin-1 bytes labeled utf-8)
	// otherwise reaches the INSERT as invalid UTF-8, Postgres rejects the
	// transaction (SQLSTATE 22021), ProcessMessage errors — and the poison
	// message would be retried UNSEEN forever, wedging the mailbox.
	bodyText := pgSafeText(strings.TrimSpace(env.Text))
	if bodyText == "" {
		bodyText = "(no content)"
	}
	bodyHTML := pgSafeText(strings.TrimSpace(env.HTML))
	if oversized > 0 {
		// Oversized ingest: raw held only the header block, the body was
		// never fetched. A notice replaces the (empty) body so agents see
		// exactly why there is no content.
		bodyText = fmt.Sprintf(
			"Message too large to ingest: %d bytes (limit %d MiB). The message body and attachments were not stored.",
			oversized, maxInboundMessageBytes>>20)
		bodyHTML = ""
	}

	inReplyTo := pgSafeText(canonicalMessageID(env.GetHeader("In-Reply-To")))
	referencesHeader := pgSafeText(strings.TrimSpace(env.GetHeader("References")))

	// Attachment blobs are streamed to storage before the transaction;
	// keys are deleted again if the transaction does not commit. (Blob
	// stores have no transactions — an orphaned blob on a crash between
	// Save and commit is harmless garbage, the reverse — a committed row
	// without its blob — would be a broken download.)
	att, skippedAtts, err := e.saveAttachments(ctx, mb, env)
	if err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			att.discard(ctx, e.blobs)
		}
	}()

	err = e.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		// (5) Sender: existing user (any role) or auto-created customer.
		// The upsert's conflict arm never changes an existing row, so an
		// address colliding with an agent/admin can never mint or modify
		// an agent account — it just attributes the article to them.
		//
		// Trust note (deliberate, documented): identity rests on the
		// unauthenticated From header — M3 performs no SPF/DKIM/ARC
		// verification, so mail spoofing an agent's address is displayed
		// as that agent's public reply (and becomes the requester of a new
		// ticket). Account state and role are never touched, bounding the
		// damage to display-level impersonation inside a thread — the same
		// trust every plain-IMAP helpdesk extends to From. Inbound header
		// authentication is a post-M3 hardening candidate.
		sender, err := q.EnsureUserByEmail(ctx, store.EnsureUserByEmailParams{
			Email: fromAddr,
			Name:  fromName,
		})
		if err != nil {
			return fmt.Errorf("ensure sender %q: %w", fromAddr, err)
		}
		// sender_type mirrors the user's actual role (M3 has no portal, so
		// a mail from an agent's address IS that agent replying). Agent
		// mail deliberately does NOT flip ticket status below.
		senderIsAgent := sender.Role == store.UserRoleAgent || sender.Role == store.UserRoleAdmin
		senderType := store.ArticleSenderCustomer
		if senderIsAgent {
			senderType = store.ArticleSenderAgent
		}

		// Load-or-create the ticket, row-locked for the status decision.
		var t store.Ticket
		if ticketID != nil {
			t, err = q.GetTicketForUpdate(ctx, *ticketID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("lock ticket: %w", err)
			}
			// A thread match on a since-deleted ticket falls through to a
			// new ticket.
		}
		if t.ID == uuid.Nil {
			t, err = e.createTicketTx(ctx, q, tx, mb, sender.ID, subject)
			if err != nil {
				return err
			}
			res.NewTicket = true
		}
		res.TicketID = t.ID

		// (4)+(7) The article, with its email headers.
		article, err := q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   t.ID,
			AuthorID:   pgtype.UUID{Bytes: sender.ID, Valid: true},
			SenderType: senderType,
			Channel:    store.ArticleChannelEmail,
			IsInternal: false,
			BodyText:   bodyText,
			BodyHtml:   e.sanitizedHTML(bodyHTML),
		})
		if err != nil {
			return fmt.Errorf("insert article: %w", err)
		}
		res.ArticleID = article.ID
		if _, err := q.SetArticleEmailMeta(ctx, store.SetArticleEmailMetaParams{
			ID:               article.ID,
			MessageID:        pgtype.Text{String: msgID, Valid: true},
			InReplyTo:        textOrNull(inReplyTo),
			ReferencesHeader: textOrNull(referencesHeader),
		}); err != nil {
			return fmt.Errorf("set article email meta: %w", err)
		}

		// (1) Authoritative dedup: INSERT … ON CONFLICT DO NOTHING. Zero
		// rows = another delivery of this Message-ID already committed
		// (concurrent worker or crash-before-\Seen redelivery); roll the
		// whole ingest back.
		rows, err := q.InsertEmailMessageID(ctx, store.InsertEmailMessageIDParams{
			MessageID: msgID,
			Direction: store.EmailDirectionInbound,
			MailboxID: pgtype.UUID{Bytes: mb.ID, Valid: true},
			TicketID:  t.ID,
			ArticleID: pgtype.UUID{Bytes: article.ID, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("record message id: %w", err)
		}
		if rows == 0 {
			return errDuplicateMessage
		}

		if err := att.insertRows(ctx, q, article.ID); err != nil {
			return err
		}

		// Over-cap attachments were dropped in saveAttachments — but never
		// silently: an internal system note in the thread tells agents
		// exactly what is missing (the log line alone is invisible to them).
		if len(skippedAtts) > 0 {
			note, err := q.CreateArticle(ctx, store.CreateArticleParams{
				TicketID:   t.ID,
				SenderType: store.ArticleSenderSystem,
				Channel:    store.ArticleChannelEmail,
				IsInternal: true,
				BodyText: fmt.Sprintf(
					"Attachment(s) dropped: this message exceeded the %d MiB attachment cap. Not stored: %s.",
					maxAttachmentBytes>>20, strings.Join(skippedAtts, ", ")),
			})
			if err != nil {
				return fmt.Errorf("insert attachment-drop note: %w", err)
			}
			if err := addTicketEvent(ctx, q, t.ID, nil, ticket.EventArticleAdded, map[string]any{
				"article_id":  note.ID,
				"sender_type": store.ArticleSenderSystem,
				"is_internal": true,
			}); err != nil {
				return err
			}
		}

		if err := addTicketEvent(ctx, q, t.ID, &sender.ID, ticket.EventArticleAdded, map[string]any{
			"article_id":  article.ID,
			"sender_type": senderType,
			"is_internal": false,
		}); err != nil {
			return err
		}

		// (6) Status. Customer mail: closed reopens (open + closed_at
		// cleared by UpdateTicketStatus), waiting_on_customer flips back
		// to open (the M2 add-article matrix). Agent-from mail never flips
		// status here: without a portal an agent emailing the mailbox is
		// unusual, and silently moving the ticket to waiting_on_customer
		// from a side channel would surprise the workspace — agents drive
		// status from the composer. Loop-guarded (suppressed) mail never
		// flips status either: machine mail must not drive workflow — an
		// out-of-office bouncing off a closed ticket must not reopen it.
		statusChanged := false
		if !senderIsAgent && !res.Suppressed && !res.NewTicket {
			var next store.TicketStatus
			switch t.Status {
			case store.TicketStatusClosed, store.TicketStatusWaitingOnCustomer:
				next = store.TicketStatusOpen
			}
			if next != "" && next != t.Status {
				if _, err := q.UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{ID: t.ID, Status: next}); err != nil {
					return fmt.Errorf("flip status: %w", err)
				}
				if err := addTicketEvent(ctx, q, t.ID, &sender.ID, ticket.EventStatusChanged, map[string]any{
					"from": t.Status, "to": next,
				}); err != nil {
					return err
				}
				if err := notifyTx(ctx, tx, ticket.NotifyTicketUpdated, t.ID); err != nil {
					return err
				}
				statusChanged = true
			}
		}
		if !res.NewTicket && !statusChanged {
			if _, err := q.TouchTicket(ctx, t.ID); err != nil {
				return fmt.Errorf("touch ticket: %w", err)
			}
		}

		// (2)+(7) Follow-up jobs — atomically with the ingest, and only
		// when the loop guard did not match: an autoresponder's mail is
		// visible in the ticket but must never generate mail back.
		if !res.Suppressed && e.river != nil {
			if res.NewTicket && mb.AutoAckEnabled {
				if _, err := e.river.InsertTx(ctx, tx, AutoAckArgs{
					TicketID:  t.ID,
					ArticleID: article.ID,
					MailboxID: mb.ID,
				}, nil); err != nil {
					return fmt.Errorf("enqueue auto-ack: %w", err)
				}
			}
			if !senderIsAgent && t.AssigneeID.Valid && uuid.UUID(t.AssigneeID.Bytes) != sender.ID {
				assignee := uuid.UUID(t.AssigneeID.Bytes)
				if _, err := e.river.InsertTx(ctx, tx, NotifyAssigneeArgs{
					TicketID:   t.ID,
					AssigneeID: assignee,
					ArticleID:  &article.ID,
					Reason:     NotifyReasonCustomerReply,
					ActorID:    &sender.ID,
					MessageID:  uuid.NewString(),
				}, nil); err != nil {
					return fmt.Errorf("enqueue notify-assignee: %w", err)
				}
			}
		}

		return notifyTx(ctx, tx, ticket.NotifyArticleCreated, t.ID)
	})
	if errors.Is(err, errDuplicateMessage) {
		return InboundResult{Duplicate: true}, nil
	}
	if err != nil {
		return res, fmt.Errorf("email: ingest %q: %w", msgID, err)
	}
	committed = true
	return res, nil
}

// createTicketTx allocates a number and creates the ticket + created event
// + ticket.created notification inside the caller's transaction. New
// tickets take the mail subject with reply/forward chains stripped; the
// requester is the sender.
func (e *Engine) createTicketTx(ctx context.Context, q *store.Queries, tx pgx.Tx, mb store.Mailbox, requesterID uuid.UUID, subject string) (store.Ticket, error) {
	day := e.now().UTC().Truncate(24 * time.Hour)
	seq, err := q.NextTicketNumber(ctx, day)
	if err != nil {
		return store.Ticket{}, fmt.Errorf("allocate number: %w", err)
	}
	cleaned := strings.TrimSpace(subjectPrefixRe.ReplaceAllString(subject, ""))
	cleaned = strings.TrimSpace(ticketTokenRe.ReplaceAllString(cleaned, ""))
	if cleaned == "" {
		cleaned = "(no subject)"
	}
	t, err := q.CreateTicket(ctx, store.CreateTicketParams{
		Number:      fmt.Sprintf("%s-%04d", day.Format("20060102"), seq),
		Subject:     cleaned,
		Status:      store.TicketStatusOpen,
		Priority:    store.TicketPriorityMedium,
		RequesterID: requesterID,
	})
	if err != nil {
		return store.Ticket{}, fmt.Errorf("insert ticket: %w", err)
	}
	if err := addTicketEvent(ctx, q, t.ID, nil, ticket.EventCreated, map[string]any{
		"number":  t.Number,
		"subject": t.Subject,
		"mailbox": mb.EmailAddress,
	}); err != nil {
		return store.Ticket{}, err
	}
	if err := notifyTx(ctx, tx, ticket.NotifyTicketCreated, t.ID); err != nil {
		return store.Ticket{}, err
	}
	return t, nil
}

// maxMessageIDLen caps an inbound Message-ID accepted for storage (see the
// comment at its use in process). Real-world ids run well under 200 bytes;
// 512 leaves generous slack while staying far below both the ~2.7KB btree
// index entry limit and the RFC 5322 998-octet header line limit that a
// replayed References id must respect.
const maxMessageIDLen = 512

// maxThreadingWalk caps how many References ancestor ids the threading
// walk will look up per message. The header is attacker-controlled: without
// a cap, a crafted multi-kilobyte References line turns into one indexed
// point lookup per id on every delivery. Real reply chains anchor within
// the newest few ids; a legitimate mail whose only known ancestor is
// deeper than the cap still threads via In-Reply-To or the subject token
// (outbound mail always stamps one).
const maxThreadingWalk = 25

// resolveThread implements threading step (3). Returns nil when the mail
// starts a new ticket.
func (e *Engine) resolveThread(ctx context.Context, env *enmime.Envelope) (*uuid.UUID, error) {
	// References, walked right-to-left: the header is oldest-first, and
	// the newest ancestor is the most reliable thread anchor.
	refs := parseMessageIDs(env.GetHeader("References"))
	if len(refs) > maxThreadingWalk {
		refs = refs[len(refs)-maxThreadingWalk:] // keep the newest ids
	}
	candidates := make([]string, 0, len(refs)+1)
	for i := len(refs) - 1; i >= 0; i-- {
		candidates = append(candidates, refs[i])
	}
	if id := canonicalMessageID(env.GetHeader("In-Reply-To")); id != "" {
		candidates = append(candidates, id)
	}
	for _, id := range candidates {
		// pgSafeText: a header id with invalid UTF-8 would fail the query
		// itself (the parameter must be valid UTF-8), erroring the ingest.
		row, err := e.q.GetEmailMessageID(ctx, pgSafeText(id))
		if err == nil {
			return &row.TicketID, nil // both directions: inbound and our outbound ids
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("email: threading lookup %q: %w", id, err)
		}
	}

	// Subject-token fallback: "[SD-20260718-0001]".
	if m := ticketTokenRe.FindStringSubmatch(env.GetHeader("Subject")); m != nil {
		row, err := e.q.GetTicketByNumber(ctx, m[1])
		if err == nil {
			return &row.Ticket.ID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("email: subject-token lookup %q: %w", m[1], err)
		}
	}
	return nil, nil
}

// ingestStub handles the hard-parse-failure path: a minimal customer-less
// ticket article so the mail is not invisible. Uses the raw bytes' hash as
// the Message-ID so redeliveries still dedup.
func (e *Engine) ingestStub(ctx context.Context, mb store.Mailbox, raw []byte) (InboundResult, error) {
	stub := enmimeStub{
		msgID:   synthesizeMessageID(raw),
		subject: "(unparseable message)",
	}
	var res InboundResult
	err := e.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		sender, err := q.EnsureUserByEmail(ctx, store.EnsureUserByEmailParams{
			Email: "unparseable@" + mailboxDomain(mb), Name: "Unknown sender",
		})
		if err != nil {
			return fmt.Errorf("ensure stub sender: %w", err)
		}
		t, err := e.createTicketTx(ctx, q, tx, mb, sender.ID, stub.subject)
		if err != nil {
			return err
		}
		res.TicketID, res.NewTicket = t.ID, true
		article, err := q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   t.ID,
			AuthorID:   pgtype.UUID{Bytes: sender.ID, Valid: true},
			SenderType: store.ArticleSenderCustomer,
			Channel:    store.ArticleChannelEmail,
			BodyText:   "This message could not be parsed as MIME. Raw size: " + fmt.Sprint(len(raw)) + " bytes.",
		})
		if err != nil {
			return fmt.Errorf("insert stub article: %w", err)
		}
		res.ArticleID = article.ID
		if _, err := q.SetArticleEmailMeta(ctx, store.SetArticleEmailMetaParams{
			ID:        article.ID,
			MessageID: pgtype.Text{String: stub.msgID, Valid: true},
		}); err != nil {
			return fmt.Errorf("set stub email meta: %w", err)
		}
		rows, err := q.InsertEmailMessageID(ctx, store.InsertEmailMessageIDParams{
			MessageID: stub.msgID,
			Direction: store.EmailDirectionInbound,
			MailboxID: pgtype.UUID{Bytes: mb.ID, Valid: true},
			TicketID:  t.ID,
			ArticleID: pgtype.UUID{Bytes: article.ID, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("record stub message id: %w", err)
		}
		if rows == 0 {
			return errDuplicateMessage
		}
		return notifyTx(ctx, tx, ticket.NotifyTicketCreated, t.ID)
	})
	if errors.Is(err, errDuplicateMessage) {
		return InboundResult{Duplicate: true}, nil
	}
	if err != nil {
		return res, fmt.Errorf("email: ingest stub: %w", err)
	}
	// Stub tickets are never acked (Suppressed for the caller's logging).
	res.Suppressed = true
	return res, nil
}

type enmimeStub struct{ msgID, subject string }

// savedAttachment tracks one extracted attachment between blob save and
// row insert.
type savedAttachment struct {
	key         string
	filename    string
	contentType string
	size        int64
}

type savedAttachments []savedAttachment

// saveAttachments streams the message's attachments — including inline
// parts carrying a Content-ID (pasted images) — to blob storage, capping
// the cumulative size at maxAttachmentBytes. Excess parts are skipped (the
// message still ingests) and reported in skipped ("name (n bytes)") so the
// ingest can leave a visible system note about them.
func (e *Engine) saveAttachments(ctx context.Context, mb store.Mailbox, env *enmime.Envelope) (out savedAttachments, skipped []string, err error) {
	var total int64

	parts := make([]*enmime.Part, 0, len(env.Attachments)+len(env.Inlines)+len(env.OtherParts))
	parts = append(parts, env.Attachments...)
	for _, p := range append(append([]*enmime.Part{}, env.Inlines...), env.OtherParts...) {
		// Inline/related parts count when they are real files: a name or a
		// cid (referenced from the HTML body). Bare inline text parts are
		// the body, not attachments.
		if p.FileName != "" || p.ContentID != "" {
			parts = append(parts, p)
		}
	}

	for _, p := range parts {
		size := int64(len(p.Content))
		if size == 0 {
			continue
		}
		name := pgSafeText(p.FileName)
		if name == "" {
			name = "inline-" + pgSafeText(p.ContentID)
		}
		if total+size > maxAttachmentBytes {
			slog.Warn("email: attachment skipped, message over cap",
				"mailbox", mb.EmailAddress, "filename", p.FileName, "size", size, "cap", maxAttachmentBytes)
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes)", name, size))
			continue
		}
		total += size

		key := storage.NewKey()
		if _, err := e.blobs.Save(ctx, key, bytes.NewReader(p.Content)); err != nil {
			out.discard(ctx, e.blobs)
			return nil, nil, fmt.Errorf("email: save attachment %q: %w", p.FileName, err)
		}
		ct := pgSafeText(p.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		out = append(out, savedAttachment{key: key, filename: name, contentType: ct, size: size})
	}
	return out, skipped, nil
}

// insertRows writes the article_attachments rows (inside the ingest tx).
func (a savedAttachments) insertRows(ctx context.Context, q *store.Queries, articleID uuid.UUID) error {
	for _, att := range a {
		if _, err := q.CreateArticleAttachment(ctx, store.CreateArticleAttachmentParams{
			ArticleID:   articleID,
			Filename:    att.filename,
			ContentType: att.contentType,
			SizeBytes:   att.size,
			StorageKey:  att.key,
		}); err != nil {
			return fmt.Errorf("insert attachment row %q: %w", att.filename, err)
		}
	}
	return nil
}

// discard best-effort deletes saved blobs after a failed/rolled-back ingest.
func (a savedAttachments) discard(ctx context.Context, blobs storage.Storage) {
	for _, att := range a {
		if err := blobs.Delete(context.WithoutCancel(ctx), att.key); err != nil {
			slog.Warn("email: orphaned attachment blob", "key", att.key, "error", err)
		}
	}
}

// isAutoResponder is the loop guard: true when the message declares itself
// machine-generated. Matching mail still ingests (visibility) but never
// triggers auto-ack or notify mail (architecture doc §4).
func isAutoResponder(env *enmime.Envelope) bool {
	if as := strings.TrimSpace(env.GetHeader("Auto-Submitted")); as != "" && !strings.EqualFold(as, "no") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(env.GetHeader("Precedence"))) {
	case "bulk", "auto_reply", "auto-reply", "list":
		return true
	}
	if strings.TrimSpace(env.GetHeader("X-Auto-Response-Suppress")) != "" {
		return true
	}
	return false
}

// synthesizeMessageID derives a stable id for mail without a Message-ID
// header: the content hash keeps redelivery dedup working (same bytes,
// same id) without trusting anything in the message.
func synthesizeMessageID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x@synthesized.slatedesk.invalid", sum[:16])
}

// parseFrom extracts display name + address from a From header, degrading
// through: parsed address -> bare trimmed value -> placeholder. Never
// fails: an unparseable sender must not wedge ingest (see ProcessMessage).
func parseFrom(header string) (name, addr string) {
	if a, err := mail.ParseAddress(header); err == nil {
		name, addr = strings.TrimSpace(a.Name), strings.TrimSpace(a.Address)
	} else if trimmed := strings.TrimSpace(header); trimmed != "" && strings.Contains(trimmed, "@") && !strings.ContainsAny(trimmed, " \t<>") {
		addr = trimmed
	}
	if addr == "" {
		addr = "unknown-sender@invalid"
	}
	if name == "" {
		if at := strings.IndexByte(addr, '@'); at > 0 {
			name = addr[:at]
		} else {
			name = addr
		}
	}
	return pgSafeText(name), pgSafeText(addr)
}

// pgSafeText coerces arbitrary decoded mail text into something Postgres
// text columns (and query parameters) accept: invalid UTF-8 sequences
// become replacement runes and NUL bytes are dropped. Mail is
// attacker-controlled and charset declarations lie; without this a single
// mislabeled body poisons its ingest transaction forever (SQLSTATE 22021),
// leaving the message UNSEEN and retried on every sweep.
func pgSafeText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s
	}
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "")
}

// parseMessageIDs extracts canonical message ids from a References-style
// header, preserving order (oldest first per RFC 5322).
func parseMessageIDs(header string) []string {
	var ids []string
	for _, m := range msgIDRe.FindAllStringSubmatch(header, -1) {
		ids = append(ids, m[1])
	}
	if ids == nil {
		// Tolerate bare ids without angle brackets.
		for f := range strings.FieldsSeq(header) {
			if strings.Contains(f, "@") {
				ids = append(ids, canonicalMessageID(f))
			}
		}
	}
	return ids
}

// mailboxDomain returns the domain of the mailbox address (Message-ID
// generation), with a safe fallback.
func mailboxDomain(mb store.Mailbox) string {
	if at := strings.LastIndexByte(mb.EmailAddress, '@'); at >= 0 && at < len(mb.EmailAddress)-1 {
		return mb.EmailAddress[at+1:]
	}
	return "slatedesk.invalid"
}

// sanitizedHTML mirrors the ticket service: bluemonday UGC policy, empty
// maps to NULL.
func (e *Engine) sanitizedHTML(html string) pgtype.Text {
	if strings.TrimSpace(html) == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: e.html.Sanitize(html), Valid: true}
}

func textOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
