package email

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// seedEmailTicket ingests one inbound mail and returns (ticketID, inbound
// message id, requester email).
func seedEmailTicket(t *testing.T, e *Engine, mb store.Mailbox) (uuid.UUID, string, string) {
	t.Helper()
	from := uniq("customer") + "@example.test"
	msgID := uniq("inbound") + "@example.test"
	res, err := e.ProcessMessage(context.Background(), mb, inboundMail{
		from:      "Customer <" + from + ">",
		subject:   "Broken printer",
		messageID: msgID,
	}.raw())
	if err != nil {
		t.Fatalf("seed inbound: %v", err)
	}
	return res.TicketID, msgID, from
}

// waitDeliveryStatus polls until the article reaches the wanted status.
func waitDeliveryStatus(t *testing.T, articleID uuid.UUID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := textVal(getArticle(t, articleID).DeliveryStatus); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("article %s delivery_status = %q, want %q after %s", articleID, got, want, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestOutboundAgentReplyEndToEnd: agent public reply through the ticket
// service -> transactional email_send enqueue -> worker -> SMTP sink, with
// every threading header correct.
func TestOutboundAgentReplyEndToEnd(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)

	mb := makeMailbox(t, mailboxOpts{
		smtpPort:    sink.port(),
		signature:   "SlateDesk Helpdesk\nMon-Fri 9-5",
		displayName: "Helpdesk",
	})
	ticketID, inboundID, customerEmail := seedEmailTicket(t, e, mb)
	agent := makeUser(t, store.UserRoleAgent)

	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	article, err := svc.AddArticle(ctx, ticketID, ticket.ArticleInput{
		AuthorID:   &agent.ID,
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		BodyText:   "We replaced the fuser. Please try again.",
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	msgs := sink.waitForMessages(t, 1, 20*time.Second)
	waitDeliveryStatus(t, article.ID, DeliverySent, 20*time.Second)

	m := msgs[0]
	tk := getTicket(t, ticketID)

	// Recipient + sender.
	if len(m.To) != 1 || m.To[0] != customerEmail {
		t.Errorf("rcpt = %v, want %s", m.To, customerEmail)
	}
	if m.From != mb.EmailAddress {
		t.Errorf("mail from = %q, want %q", m.From, mb.EmailAddress)
	}
	if !strings.Contains(m.header("From"), "Helpdesk") {
		t.Errorf("From display = %q", m.header("From"))
	}

	// Subject: Re: + token.
	subj := m.header("Subject")
	if !strings.HasPrefix(subj, "Re: ") || !strings.Contains(subj, "[SD-"+tk.Number+"]") {
		t.Errorf("subject = %q", subj)
	}

	// Message-ID on the wire == the id recorded in email_message_ids and
	// on the article row.
	wireID := canonicalMessageID(m.header("Message-ID"))
	a := getArticle(t, article.ID)
	if wireID == "" || wireID != textVal(a.MessageID) {
		t.Errorf("wire Message-ID %q != article message_id %q", wireID, textVal(a.MessageID))
	}
	row, err := store.New(testPool).GetEmailMessageID(ctx, wireID)
	if err != nil {
		t.Fatalf("outbound id not recorded: %v", err)
	}
	if row.Direction != store.EmailDirectionOutbound || row.TicketID != ticketID {
		t.Errorf("outbound id row = %+v", row)
	}

	// Threading headers point at the inbound mail.
	if got := canonicalMessageID(m.header("In-Reply-To")); got != inboundID {
		t.Errorf("In-Reply-To = %q, want %q", got, inboundID)
	}
	if refs := m.header("References"); !strings.Contains(refs, "<"+inboundID+">") {
		t.Errorf("References = %q, want to contain %q", refs, inboundID)
	}

	// Signature appended; human mail carries NO loop-guard headers.
	if !strings.Contains(m.Data, "SlateDesk Helpdesk") {
		t.Errorf("signature missing from body")
	}
	if m.header("Auto-Submitted") != "" || m.header("X-Auto-Response-Suppress") != "" {
		t.Errorf("agent reply must not carry auto headers (got %q / %q)",
			m.header("Auto-Submitted"), m.header("X-Auto-Response-Suppress"))
	}
}

// TestOutboundMessageIDRecordedBeforeSend: when the SMTP dial fails, the
// Message-ID and 'sending' status are ALREADY committed — the pre-send
// ordering invariant, observed directly.
func TestOutboundMessageIDRecordedBeforeSend(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	// Port 1: nothing listens; the send must fail after the pre-send tx.
	mb := makeMailbox(t, mailboxOpts{smtpPort: 1})
	ticketID, _, _ := seedEmailTicket(t, e, mb)
	agent := makeUser(t, store.UserRoleAgent)

	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	article, err := svc.AddArticle(ctx, ticketID, ticket.ArticleInput{
		AuthorID:   &agent.ID,
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		BodyText:   "This send will fail.",
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	waitDeliveryStatus(t, article.ID, DeliveryQueued, 5*time.Second)

	if err := e.BuildAndSend(ctx, article.ID, mb.ID, false); err == nil {
		t.Fatalf("BuildAndSend against a dead port must fail")
	}

	a := getArticle(t, article.ID)
	msgID := textVal(a.MessageID)
	if msgID == "" {
		t.Fatalf("message_id not recorded despite failed send")
	}
	if !strings.HasSuffix(msgID, "@"+mailboxDomain(mb)) {
		t.Errorf("message id %q not under mailbox domain %q", msgID, mailboxDomain(mb))
	}
	if got := textVal(a.DeliveryStatus); got != DeliverySending {
		t.Errorf("delivery_status = %q, want sending (pre-send committed)", got)
	}
	if _, err := store.New(testPool).GetEmailMessageID(ctx, msgID); err != nil {
		t.Errorf("outbound message id row missing after failed send: %v", err)
	}

	// Retry against a live sink reuses the SAME Message-ID.
	sink := newSMTPSink(t)
	if _, err := testPool.Exec(ctx, "UPDATE mailboxes SET smtp_port = $2 WHERE id = $1", mb.ID, sink.port()); err != nil {
		t.Fatalf("repoint mailbox: %v", err)
	}
	if err := e.BuildAndSend(ctx, article.ID, mb.ID, false); err != nil {
		t.Fatalf("retry BuildAndSend: %v", err)
	}
	msgs := sink.waitForMessages(t, 1, 10*time.Second)
	if got := canonicalMessageID(msgs[0].header("Message-ID")); got != msgID {
		t.Errorf("retry used %q, want the pre-committed %q", got, msgID)
	}
	waitDeliveryStatus(t, article.ID, DeliverySent, 5*time.Second)
}

// TestOutboundCrashWindowIdempotency: delivery_status=sent makes
// BuildAndSend a no-op (River retry after crash-post-sent-commit).
func TestOutboundCrashWindowIdempotency(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{smtpPort: sink.port()})
	ticketID, _, _ := seedEmailTicket(t, e, mb)

	// Hand-made outbound article already marked sent.
	var article store.Article
	err := e.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		var err error
		article, err = q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   ticketID,
			SenderType: store.ArticleSenderAgent,
			Channel:    store.ArticleChannelWeb,
			BodyText:   "already delivered",
		})
		return err
	})
	if err != nil {
		t.Fatalf("create article: %v", err)
	}
	if _, err := store.New(testPool).SetArticleDeliveryStatus(ctx, store.SetArticleDeliveryStatusParams{
		ID: article.ID, DeliveryStatus: textOrNull(DeliverySent),
	}); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	if err := e.BuildAndSend(ctx, article.ID, mb.ID, false); err != nil {
		t.Fatalf("BuildAndSend on sent article: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(sink.messages()); n != 0 {
		t.Errorf("no-op send still delivered %d messages", n)
	}
}

// TestEmailSendWorkerTerminalFailure: the last allowed attempt marks the
// article failed (exercised directly, without waiting out River backoff).
func TestEmailSendWorkerTerminalFailure(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{smtpPort: 1}) // dead port
	ticketID, _, _ := seedEmailTicket(t, e, mb)

	var article store.Article
	err := e.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		var err error
		article, err = q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   ticketID,
			SenderType: store.ArticleSenderAgent,
			Channel:    store.ArticleChannelWeb,
			BodyText:   "doomed",
		})
		return err
	})
	if err != nil {
		t.Fatalf("create article: %v", err)
	}

	worker := &EmailSendWorker{engine: e}
	job := &river.Job[EmailSendArgs]{
		JobRow: &rivertype.JobRow{Attempt: emailSendMaxAttempts, MaxAttempts: emailSendMaxAttempts},
		Args: EmailSendArgs{
			ArticleID: article.ID,
			TicketID:  ticketID,
			MailboxID: mb.ID,
		},
	}
	if err := worker.Work(ctx, job); err == nil {
		t.Fatalf("terminal attempt should still return the error to River")
	}
	if got := textVal(getArticle(t, article.ID).DeliveryStatus); got != DeliveryFailed {
		t.Errorf("delivery_status = %q, want failed", got)
	}
}

// TestAutoAckFlow: inbound -> auto-ack worker -> system article + send;
// the ack carries the loop-guard headers, and feeding it back through the
// inbound pipeline is suppressed (never ack an ack).
func TestAutoAckFlow(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{smtpPort: sink.port(), autoAck: true})

	from := uniq("newbie") + "@example.test"
	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Newbie <" + from + ">",
		subject:   "Please help",
		messageID: uniq("first") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}

	msgs := sink.waitForMessages(t, 1, 20*time.Second)
	ack := msgs[0]
	tk := getTicket(t, res.TicketID)

	if len(ack.To) != 1 || ack.To[0] != from {
		t.Errorf("ack rcpt = %v, want %s", ack.To, from)
	}
	if !strings.Contains(ack.header("Subject"), "[SD-"+tk.Number+"]") {
		t.Errorf("ack subject = %q lacks token", ack.header("Subject"))
	}
	if got := ack.header("Auto-Submitted"); got != "auto-replied" {
		t.Errorf("ack Auto-Submitted = %q", got)
	}
	if ack.header("X-Auto-Response-Suppress") == "" {
		t.Errorf("ack lacks X-Auto-Response-Suppress")
	}
	if !strings.Contains(ack.Data, "SD-"+tk.Number) {
		t.Errorf("ack body lacks ticket number")
	}

	// The ack is a system article on the ticket with a delivery badge.
	var ackArticleID uuid.UUID
	if err := testPool.QueryRow(ctx,
		"SELECT id FROM articles WHERE ticket_id = $1 AND sender_type = 'system' AND channel = 'email'",
		res.TicketID).Scan(&ackArticleID); err != nil {
		t.Fatalf("ack article: %v", err)
	}
	waitDeliveryStatus(t, ackArticleID, DeliverySent, 20*time.Second)

	// Never ack an ack, layer 1: our own ack bouncing back verbatim is a
	// known Message-ID — dedup short-circuits before anything happens.
	before := len(listJobs(t, AutoAckArgs{}.Kind()))
	bounce, err := e.ProcessMessage(ctx, mb, []byte(ack.Data))
	if err != nil {
		t.Fatalf("re-ingest ack verbatim: %v", err)
	}
	if !bounce.Duplicate {
		t.Errorf("verbatim ack bounce not deduped: %+v", bounce)
	}

	// Never ack an ack, layer 2: a REMOTE autoresponder answering our ack
	// (fresh Message-ID, Auto-Submitted set, threads via our ack's id).
	ackID := canonicalMessageID(ack.header("Message-ID"))
	autoReply := inboundMail{
		from:      "Newbie <" + from + ">",
		subject:   "Automatic reply: " + ack.header("Subject"),
		messageID: uniq("remote-ooo") + "@example.test",
		headers: []string{
			"In-Reply-To: <" + ackID + ">",
			"Auto-Submitted: auto-replied",
		},
		body: "I am out of the office.\n",
	}
	backRes, err := e.ProcessMessage(ctx, mb, autoReply.raw())
	if err != nil {
		t.Fatalf("ingest remote autoreply: %v", err)
	}
	if !backRes.Suppressed {
		t.Errorf("remote autoreply not suppressed: %+v", backRes)
	}
	if backRes.NewTicket || backRes.TicketID != res.TicketID {
		t.Errorf("remote autoreply did not thread via our ack id: %+v", backRes)
	}
	if after := len(listJobs(t, AutoAckArgs{}.Kind())); after != before {
		t.Errorf("ack of an ack was enqueued (%d -> %d)", before, after)
	}
}

// TestAssignNotifyFlow: assignment through the ticket service notifies the
// assignee by email (threaded, auto-headers), and self-assignment stays
// silent.
func TestAssignNotifyFlow(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{smtpPort: sink.port()})
	ticketID, inboundID, _ := seedEmailTicket(t, e, mb)

	agent := makeUser(t, store.UserRoleAgent)
	admin := makeUser(t, store.UserRoleAdmin)

	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	if _, err := svc.Assign(ctx, ticketID, &agent.ID, &admin.ID); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	msgs := sink.waitForMessages(t, 1, 20*time.Second)
	m := msgs[0]
	tk := getTicket(t, ticketID)

	if len(m.To) != 1 || m.To[0] != agent.Email {
		t.Errorf("notify rcpt = %v, want %s", m.To, agent.Email)
	}
	if !strings.Contains(m.header("Subject"), "[SD-"+tk.Number+"]") {
		t.Errorf("notify subject = %q", m.header("Subject"))
	}
	if m.header("Auto-Submitted") != "auto-replied" {
		t.Errorf("notify Auto-Submitted = %q", m.header("Auto-Submitted"))
	}
	if got := canonicalMessageID(m.header("In-Reply-To")); got != inboundID {
		t.Errorf("notify In-Reply-To = %q, want %q", got, inboundID)
	}
	// The notification's Message-ID is recorded (article NULL) so replies
	// to it thread into the ticket.
	row, err := store.New(testPool).GetEmailMessageID(ctx, canonicalMessageID(m.header("Message-ID")))
	if err != nil {
		t.Fatalf("notification id not recorded: %v", err)
	}
	if row.TicketID != ticketID || row.ArticleID.Valid {
		t.Errorf("notification id row = %+v (want ticket %s, article NULL)", row, ticketID)
	}

	// Self-assignment: no job, no mail.
	ticket2, _, _ := seedEmailTicket(t, e, mb)
	before := len(listJobs(t, NotifyAssigneeArgs{}.Kind()))
	if _, err := svc.Assign(ctx, ticket2, &agent.ID, &agent.ID); err != nil {
		t.Fatalf("self-assign: %v", err)
	}
	if after := len(listJobs(t, NotifyAssigneeArgs{}.Kind())); after != before {
		t.Errorf("self-assignment enqueued a notification (%d -> %d)", before, after)
	}
}

// TestNonEmailTicketUnchanged: a web-origin ticket with the mailer wired
// behaves exactly as in M2 — the agent reply enqueues nothing.
func TestNonEmailTicketUnchanged(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	clearJobs(t)

	customer := makeUser(t, store.UserRoleCustomer)
	agent := makeUser(t, store.UserRoleAgent)

	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	tk, _, err := svc.CreateTicket(ctx, ticket.CreateTicketParams{
		Subject:     "Web ticket",
		RequesterID: customer.ID,
		Article: ticket.ArticleInput{
			AuthorID:   &customer.ID,
			SenderType: store.ArticleSenderCustomer,
			Channel:    store.ArticleChannelWeb,
			BodyText:   "help me",
		},
	})
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	article, err := svc.AddArticle(ctx, tk.ID, ticket.ArticleInput{
		AuthorID:   &agent.ID,
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		BodyText:   "sure thing",
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}

	if n := len(listJobs(t, EmailSendArgs{}.Kind())); n != 0 {
		t.Errorf("email_send jobs for a web ticket = %d, want 0", n)
	}
	if got := textVal(getArticle(t, article.ID).DeliveryStatus); got != "" {
		t.Errorf("web reply delivery_status = %q, want NULL", got)
	}
	// M2 status matrix still applies (agent public reply flips to
	// waiting_on_customer).
	if got := getTicket(t, tk.ID).Status; got != store.TicketStatusWaitingOnCustomer {
		t.Errorf("status = %q, want waiting_on_customer", got)
	}
}

// TestMarkSendFailedKeepsSent: a race between success and the terminal
// failure path must never downgrade 'sent'.
func TestMarkSendFailedKeepsSent(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})
	ticketID, _, _ := seedEmailTicket(t, e, mb)

	var article store.Article
	err := e.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		var err error
		article, err = q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   ticketID,
			SenderType: store.ArticleSenderAgent,
			Channel:    store.ArticleChannelWeb,
			BodyText:   "sent already",
		})
		if err != nil {
			return err
		}
		_, err = q.SetArticleDeliveryStatus(ctx, store.SetArticleDeliveryStatusParams{
			ID: article.ID, DeliveryStatus: textOrNull(DeliverySent),
		})
		return err
	})
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := e.MarkSendFailed(ctx, article.ID); err != nil {
		t.Fatalf("MarkSendFailed: %v", err)
	}
	if got := textVal(getArticle(t, article.ID).DeliveryStatus); got != DeliverySent {
		t.Errorf("delivery_status = %q, want sent (never downgraded)", got)
	}
}

// TestAutoAckDespiteInternalNote: the internal attachment-drop system note
// (sender_type=system, channel=email, is_internal=true) must NOT trip the
// auto-ack idempotency guard — only the public ack counts — and the guard
// still makes a redelivered ack job a no-op.
func TestAutoAckDespiteInternalNote(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{autoAck: true})

	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Dropper <" + uniq("dropper") + "@example.test>",
		subject:   "Mail whose attachments blew the cap",
		messageID: uniq("dropped") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("seed inbound: %v", err)
	}

	// Simulate the attachment-drop note the ingest writes for over-cap
	// mail: internal system email article on the same ticket.
	if _, err := store.New(testPool).CreateArticle(ctx, store.CreateArticleParams{
		TicketID:   res.TicketID,
		SenderType: store.ArticleSenderSystem,
		Channel:    store.ArticleChannelEmail,
		IsInternal: true,
		BodyText:   "Attachment(s) dropped: this message exceeded the cap.",
	}); err != nil {
		t.Fatalf("insert drop note: %v", err)
	}

	worker := &AutoAckWorker{engine: e}
	job := &river.Job[AutoAckArgs]{
		JobRow: &rivertype.JobRow{},
		Args: AutoAckArgs{
			TicketID:  res.TicketID,
			ArticleID: res.ArticleID,
			MailboxID: mb.ID,
		},
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("AutoAckWorker: %v", err)
	}

	countPublicAcks := func() int {
		var n int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM articles
			WHERE ticket_id = $1 AND sender_type = 'system'
			  AND channel = 'email' AND is_internal = false`,
			res.TicketID).Scan(&n); err != nil {
			t.Fatalf("count acks: %v", err)
		}
		return n
	}
	if n := countPublicAcks(); n != 1 {
		t.Fatalf("public ack articles = %d, want 1 (internal note must not suppress the ack)", n)
	}

	// Redelivery of the ack job stays a no-op (real idempotency guard).
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("redelivered AutoAckWorker: %v", err)
	}
	if n := countPublicAcks(); n != 1 {
		t.Errorf("public ack articles after redelivery = %d, want 1", n)
	}
}

// TestRetryStuckSending: an article stranded on 'sending' with no live
// email_send job (crash during the final attempt, job discarded by the
// rescuer) can be re-queued via RetryFailedSend; while a live job exists
// the same call answers ErrRetryNotFailed.
func TestRetryStuckSending(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{})
	ticketID, _, _ := seedEmailTicket(t, e, mb)

	var article store.Article
	err := e.inTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		var err error
		article, err = q.CreateArticle(ctx, store.CreateArticleParams{
			TicketID:   ticketID,
			SenderType: store.ArticleSenderAgent,
			Channel:    store.ArticleChannelWeb,
			BodyText:   "stranded mid-send",
		})
		return err
	})
	if err != nil {
		t.Fatalf("create article: %v", err)
	}
	// Simulate the pre-send commit of the crashed final attempt: outbound
	// Message-ID recorded + badge on 'sending'; the job is gone.
	msgID := uniq("stranded") + "@" + mailboxDomain(mb)
	if _, err := store.New(testPool).SetArticleEmailMeta(ctx, store.SetArticleEmailMetaParams{
		ID:             article.ID,
		MessageID:      textOrNull(msgID),
		DeliveryStatus: textOrNull(DeliverySending),
	}); err != nil {
		t.Fatalf("stamp sending: %v", err)
	}
	if _, err := store.New(testPool).InsertEmailMessageID(ctx, store.InsertEmailMessageIDParams{
		MessageID: msgID,
		Direction: store.EmailDirectionOutbound,
		MailboxID: pgtype.UUID{Bytes: mb.ID, Valid: true},
		TicketID:  ticketID,
		ArticleID: pgtype.UUID{Bytes: article.ID, Valid: true},
	}); err != nil {
		t.Fatalf("record outbound id: %v", err)
	}

	// No live job: the stuck badge is retryable.
	updated, err := e.RetryFailedSend(ctx, article.ID)
	if err != nil {
		t.Fatalf("RetryFailedSend on stuck sending: %v", err)
	}
	if got := textVal(updated.DeliveryStatus); got != DeliveryQueued {
		t.Errorf("delivery_status = %q, want queued", got)
	}

	// The retry enqueued a live email_send job; strand the badge again —
	// now the same call must refuse (send would be genuinely in flight).
	if _, err := store.New(testPool).SetArticleDeliveryStatus(ctx, store.SetArticleDeliveryStatusParams{
		ID: article.ID, DeliveryStatus: textOrNull(DeliverySending),
	}); err != nil {
		t.Fatalf("re-stamp sending: %v", err)
	}
	if _, err := e.RetryFailedSend(ctx, article.ID); !errors.Is(err, ErrRetryNotFailed) {
		t.Errorf("retry with live job = %v, want ErrRetryNotFailed", err)
	}
}

// TestNotifyRedeliveryStableMessageID: a redelivered notify-assignee job
// re-sends with the SAME Message-ID (minted at enqueue time), so MUAs
// collapse the duplicate and only one outbound id row ever exists.
func TestNotifyRedeliveryStableMessageID(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{smtpPort: sink.port()})
	ticketID, _, _ := seedEmailTicket(t, e, mb)

	agent := makeUser(t, store.UserRoleAgent)
	if _, err := testPool.Exec(ctx,
		"UPDATE tickets SET assignee_id = $1 WHERE id = $2", agent.ID, ticketID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	worker := &NotifyAssigneeWorker{engine: e}
	job := &river.Job[NotifyAssigneeArgs]{
		JobRow: &rivertype.JobRow{},
		Args: NotifyAssigneeArgs{
			TicketID:   ticketID,
			AssigneeID: agent.ID,
			Reason:     NotifyReasonAssigned,
			MessageID:  uuid.NewString(), // as minted at enqueue time
		},
	}
	// First delivery, then a redelivery (crash after SMTP accept).
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("redelivered notify: %v", err)
	}

	msgs := sink.waitForMessages(t, 2, 20*time.Second)
	want := job.Args.MessageID + "@" + mailboxDomain(mb)
	for i, m := range msgs {
		if got := canonicalMessageID(m.header("Message-ID")); got != want {
			t.Errorf("delivery %d Message-ID = %q, want %q", i, got, want)
		}
	}
	var n int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM email_message_ids WHERE message_id = $1", want).Scan(&n); err != nil {
		t.Fatalf("count id rows: %v", err)
	}
	if n != 1 {
		t.Errorf("email_message_ids rows for notification id = %d, want 1", n)
	}
}
