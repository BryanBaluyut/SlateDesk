package email

// The M3 threading torture-suite — THE release gate (architecture doc §7:
// "an automated threading torture-suite (replies-to-replies, redelivery,
// autoresponders, closed-ticket reopen) must pass before M4").
//
// Everything runs in-process: ProcessMessage is driven directly with raw
// RFC 5322 bytes (the exact bytes an IMAP fetch yields), outbound mail is
// observed on the smtpSink, and the crash-window scenario uses the
// imapserver memory backend + a real Supervisor. The suite is written
// adversarially: References that lie, forged subject tokens, autoresponder
// storms, oversized and hostile MIME, self-loops, and redeliveries at every
// awkward moment. Each test asserts exact ticket/article/message-id/status/
// job outcomes.
//
// Run: DATABASE_URL=... go test ./internal/email -run TestTorture -count=2

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// ---------------------------------------------------------------------------
// Torture-suite helpers
// ---------------------------------------------------------------------------

// tortureArticle is a slim articles-row snapshot for exact-order assertions.
type tortureArticle struct {
	ID         uuid.UUID
	SenderType string
	Channel    string
	IsInternal bool
	BodyText   string
	MessageID  string
}

// listTicketArticles returns a ticket's articles oldest-first.
func listTicketArticles(t *testing.T, ticketID uuid.UUID) []tortureArticle {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id, sender_type::text, channel::text, is_internal, body_text, COALESCE(message_id, '')
		FROM articles WHERE ticket_id = $1 ORDER BY created_at, id`, ticketID)
	if err != nil {
		t.Fatalf("list ticket articles: %v", err)
	}
	defer rows.Close()
	var out []tortureArticle
	for rows.Next() {
		var a tortureArticle
		if err := rows.Scan(&a.ID, &a.SenderType, &a.Channel, &a.IsInternal, &a.BodyText, &a.MessageID); err != nil {
			t.Fatalf("scan article: %v", err)
		}
		out = append(out, a)
	}
	return out
}

func countTicketArticles(t *testing.T, ticketID uuid.UUID) int {
	t.Helper()
	return len(listTicketArticles(t, ticketID))
}

// countMessageIDRows counts email_message_ids rows for a ticket (optionally
// one direction; "" = both).
func countMessageIDRows(t *testing.T, ticketID uuid.UUID, direction string) int {
	t.Helper()
	q := "SELECT count(*) FROM email_message_ids WHERE ticket_id = $1"
	args := []any{ticketID}
	if direction != "" {
		q += " AND direction = $2"
		args = append(args, direction)
	}
	var n int
	if err := testPool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count message ids: %v", err)
	}
	return n
}

// countEvents counts a ticket's events of one type.
func countEvents(t *testing.T, ticketID uuid.UUID, typ string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM ticket_events WHERE ticket_id = $1 AND type = $2",
		ticketID, typ).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// assertOutboundRecorded asserts a wire Message-ID landed in
// email_message_ids as OUTBOUND for the given ticket.
func assertOutboundRecorded(t *testing.T, msgID string, ticketID uuid.UUID) {
	t.Helper()
	if msgID == "" {
		t.Fatalf("empty outbound message id")
	}
	row, err := store.New(testPool).GetEmailMessageID(context.Background(), msgID)
	if err != nil {
		t.Fatalf("outbound id %q not recorded: %v", msgID, err)
	}
	if row.Direction != store.EmailDirectionOutbound || row.TicketID != ticketID {
		t.Errorf("outbound id row = %+v, want outbound on ticket %s", row, ticketID)
	}
}

// timeBox fails the test if fn does not return within d (the anti-hang
// harness for the hostile-MIME scenarios). The result of fn is returned.
func timeBox(t *testing.T, d time.Duration, what string, fn func() (InboundResult, error)) (InboundResult, error) {
	t.Helper()
	type outcome struct {
		res InboundResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := fn()
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(d):
		t.Fatalf("%s did not finish within %s (hang)", what, d)
		return InboundResult{}, nil
	}
}

// assertSinkStays asserts the sink still holds exactly n messages after a
// grace window (long enough for a wrongly enqueued River job to have run).
func assertSinkStays(t *testing.T, s *smtpSink, n int, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	if got := len(s.messages()); got != n {
		t.Errorf("smtp sink has %d messages, want it to stay at %d", got, n)
	}
}

// requester loads a ticket's requester user row.
func requester(t *testing.T, ticketID uuid.UUID) store.User {
	t.Helper()
	tk := getTicket(t, ticketID)
	u, err := store.New(testPool).GetUserByID(context.Background(), tk.RequesterID)
	if err != nil {
		t.Fatalf("load requester: %v", err)
	}
	return u
}

// ---------------------------------------------------------------------------
// Scenario 1 — chain depth
// ---------------------------------------------------------------------------

// TestTortureChainDepth: original mail -> auto-ack -> customer replies to
// the ack -> agent replies -> customer replies to the agent reply quoting
// ONLY the OLDEST References entry. One ticket, exact article order, every
// outbound Message-ID recorded in email_message_ids.
func TestTortureChainDepth(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{smtpPort: sink.port(), autoAck: true})

	from := "Chain Customer <" + uniq("chain") + "@example.test>"
	aID := uniq("chain-a") + "@example.test"

	res1, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "Deep chain torture", messageID: aID,
	}.raw())
	if err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if !res1.NewTicket || res1.Suppressed {
		t.Fatalf("A result: %+v", res1)
	}
	tid := res1.TicketID

	// Auto-ack goes out; its wire Message-ID is our outbound anchor.
	ack := sink.waitForMessages(t, 1, 20*time.Second)[0]
	ackID := canonicalMessageID(ack.header("Message-ID"))
	assertOutboundRecorded(t, ackID, tid)

	// Customer replies TO THE ACK.
	res2, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "Re: " + ack.header("Subject"),
		messageID: uniq("chain-b") + "@example.test",
		headers:   []string{"In-Reply-To: <" + ackID + ">", "References: <" + aID + "> <" + ackID + ">"},
		body:      "replying to your robot\n",
	}.raw())
	if err != nil {
		t.Fatalf("ingest reply-to-ack: %v", err)
	}
	if res2.NewTicket || res2.TicketID != tid {
		t.Fatalf("reply to ack forked the thread: %+v (want %s)", res2, tid)
	}

	// Agent replies through the ticket service (transactional enqueue).
	agent := makeUser(t, store.UserRoleAgent)
	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	agentArticle, err := svc.AddArticle(ctx, tid, ticket.ArticleInput{
		AuthorID:   &agent.ID,
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		BodyText:   "chain-depth agent answer",
	})
	if err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	msgs := sink.waitForMessages(t, 2, 20*time.Second)
	var agentWire sinkMessage
	for _, m := range msgs {
		if strings.Contains(m.Data, "chain-depth agent answer") {
			agentWire = m
		}
	}
	agentWireID := canonicalMessageID(agentWire.header("Message-ID"))
	assertOutboundRecorded(t, agentWireID, tid)
	waitDeliveryStatus(t, agentArticle.ID, DeliverySent, 20*time.Second)
	if got := textVal(getArticle(t, agentArticle.ID).MessageID); got != agentWireID {
		t.Errorf("agent article message_id %q != wire %q", got, agentWireID)
	}

	// Customer's final reply quotes ONLY the OLDEST References entry (the
	// very first inbound id) — a real-world mangling MUA.
	res3, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "Re: mangled beyond recognition",
		messageID: uniq("chain-c") + "@example.test",
		headers:   []string{"References: <" + aID + ">"},
		body:      "still broken\n",
	}.raw())
	if err != nil {
		t.Fatalf("ingest oldest-ref reply: %v", err)
	}
	if res3.NewTicket || res3.TicketID != tid {
		t.Fatalf("oldest-References reply forked the thread: %+v (want %s)", res3, tid)
	}

	// Exact article order: customer A, system ack, customer reply-to-ack,
	// agent reply, customer final.
	arts := listTicketArticles(t, tid)
	wantOrder := []string{"customer", "system", "customer", "agent", "customer"}
	if len(arts) != len(wantOrder) {
		t.Fatalf("articles = %d, want %d: %+v", len(arts), len(wantOrder), arts)
	}
	for i, want := range wantOrder {
		if arts[i].SenderType != want {
			t.Errorf("article[%d].sender_type = %q, want %q", i, arts[i].SenderType, want)
		}
	}

	// Every message of the conversation is in email_message_ids: 3 inbound
	// + 2 outbound (ack, agent reply).
	if n := countMessageIDRows(t, tid, "inbound"); n != 3 {
		t.Errorf("inbound message-id rows = %d, want 3", n)
	}
	if n := countMessageIDRows(t, tid, "outbound"); n != 2 {
		t.Errorf("outbound message-id rows = %d, want 2", n)
	}

	// Exactly one auto-ack ever; status open (last word was the customer's).
	if n := len(listJobs(t, AutoAckArgs{}.Kind())); n != 1 {
		t.Errorf("auto-ack jobs = %d, want 1", n)
	}
	if got := getTicket(t, tid).Status; got != store.TicketStatusOpen {
		t.Errorf("final status = %q, want open", got)
	}
}

// ---------------------------------------------------------------------------
// Scenario 2 — References vs In-Reply-To disagree
// ---------------------------------------------------------------------------

// TestTortureReferencesBeatInReplyTo: References' NEWEST entry resolves to
// ticket X while In-Reply-To points at ticket Y — References must win.
func TestTortureReferencesBeatInReplyTo(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	xID := uniq("x-seed") + "@example.test"
	resX, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: "X <" + uniq("x") + "@example.test>", subject: "Ticket X", messageID: xID,
	}.raw())
	if err != nil {
		t.Fatalf("seed X: %v", err)
	}
	yID := uniq("y-seed") + "@example.test"
	resY, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: "Y <" + uniq("y") + "@example.test>", subject: "Ticket Y", messageID: yID,
	}.raw())
	if err != nil {
		t.Fatalf("seed Y: %v", err)
	}

	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Liar <" + uniq("liar") + "@example.test>",
		subject:   "Re: contradictory headers",
		messageID: uniq("contra") + "@example.test",
		headers: []string{
			"References: <never-seen-" + uniq("z") + "@else.test> <" + xID + ">",
			"In-Reply-To: <" + yID + ">",
		},
	}.raw())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.NewTicket {
		t.Fatalf("contradictory mail opened a new ticket: %+v", res)
	}
	if res.TicketID != resX.TicketID {
		t.Fatalf("threaded to %s, want ticket X %s (References newest-first must beat In-Reply-To)", res.TicketID, resX.TicketID)
	}
	if n := countTicketArticles(t, resY.TicketID); n != 1 {
		t.Errorf("ticket Y grew to %d articles, want 1", n)
	}
	if n := countTicketArticles(t, resX.TicketID); n != 2 {
		t.Errorf("ticket X has %d articles, want 2", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3 — triple redelivery, once after close
// ---------------------------------------------------------------------------

// TestTortureTripleRedelivery: the same Message-ID delivered three times —
// the third AFTER its ticket was closed. One article, no reopen on the
// duplicate, exactly one auto-ack enqueue.
func TestTortureTripleRedelivery(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	clearJobs(t)
	mb := makeMailbox(t, mailboxOpts{autoAck: true})

	msgID := uniq("triple") + "@example.test"
	raw := inboundMail{
		from: "Repeat <" + uniq("rep") + "@example.test>", subject: "Delivered thrice", messageID: msgID,
	}.raw()

	res1, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if !res1.NewTicket {
		t.Fatalf("first delivery: %+v", res1)
	}

	res2, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	if !res2.Duplicate {
		t.Fatalf("second delivery not deduped: %+v", res2)
	}

	if _, err := store.New(testPool).UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
		ID: res1.TicketID, Status: store.TicketStatusClosed,
	}); err != nil {
		t.Fatalf("close: %v", err)
	}

	res3, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("third delivery: %v", err)
	}
	if !res3.Duplicate {
		t.Fatalf("post-close delivery not deduped: %+v", res3)
	}

	tk := getTicket(t, res1.TicketID)
	if tk.Status != store.TicketStatusClosed || !tk.ClosedAt.Valid {
		t.Errorf("duplicate delivery reopened the ticket: status=%q closed_at.valid=%v", tk.Status, tk.ClosedAt.Valid)
	}
	if n := countTicketArticles(t, res1.TicketID); n != 1 {
		t.Errorf("articles = %d, want 1", n)
	}
	if n := countMessageIDRows(t, res1.TicketID, "inbound"); n != 1 {
		t.Errorf("inbound id rows = %d, want 1", n)
	}
	if n := len(listJobs(t, AutoAckArgs{}.Kind())); n != 1 {
		t.Errorf("auto-ack jobs = %d, want 1 (no duplicate acks)", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario 4 — crash window: committed but not \Seen
// ---------------------------------------------------------------------------

// TestTortureCrashWindowRedelivery: the ingest transaction commits, then
// the process "crashes" BEFORE storing \Seen (simulated by processing the
// raw bytes directly with no IMAP flag write). The next poll redelivers the
// UNSEEN message: dedup must skip it AND finally set \Seen — exactly one
// article ever.
func TestTortureCrashWindowRedelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)

	addr := uniq("crashbox") + "@slatedesk.test"
	f := newFakeIMAP(t, addr, "secret")
	mb := makeMailbox(t, mailboxOpts{
		address: addr, imapHost: "127.0.0.1", imapPort: int32(f.addr.Port),
	})

	raw := inboundMail{
		from:      "Crash <" + uniq("crash") + "@example.test>",
		subject:   "Crash window",
		messageID: uniq("crash") + "@example.test",
	}.raw()
	f.deliver(t, raw)

	// "First delivery": pipeline commits… and the process dies before the
	// \Seen store. ProcessMessage without the flag write IS that state.
	res1, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("simulated first delivery: %v", err)
	}
	if !res1.NewTicket {
		t.Fatalf("first delivery: %+v", res1)
	}
	status, err := f.user.Status("INBOX", &imap.StatusOptions{NumUnseen: true})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.NumUnseen == nil || *status.NumUnseen != 1 {
		t.Fatalf("fixture: message should still be UNSEEN, unseen=%v", status.NumUnseen)
	}

	// "Restart": the supervisor adopts the mailbox and sweeps the UNSEEN
	// message again — the redelivery.
	sup := NewSupervisor(e, fastConfig())
	go func() { _ = sup.Run(ctx) }()

	waitFor(t, 20*time.Second, "\\Seen set on redelivered duplicate", func() bool {
		st, err := f.user.Status("INBOX", &imap.StatusOptions{NumUnseen: true})
		return err == nil && st.NumUnseen != nil && *st.NumUnseen == 0
	})

	// Let a couple more sweeps pass, then assert exactly-once.
	time.Sleep(3 * fastConfig().PollInterval)
	if n := inboundCount(t, mb.ID); n != 1 {
		t.Errorf("inbound id rows = %d, want 1 (dedup on redelivery)", n)
	}
	if n := countTicketArticles(t, res1.TicketID); n != 1 {
		t.Errorf("articles = %d, want exactly 1", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario 5 — autoresponder storm + ack bounce
// ---------------------------------------------------------------------------

// TestTortureAutoresponderStorm: five out-of-office variants all ingest
// (visibility) but are suppressed — ZERO auto-ack and ZERO notify jobs,
// even against an assigned ticket.
func TestTortureAutoresponderStorm(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	clearJobs(t)
	mb := makeMailbox(t, mailboxOpts{autoAck: true})

	variants := [][]string{
		{"Auto-Submitted: auto-replied"},
		{"Precedence: bulk"},
		{"Precedence: list"},
		{"X-Auto-Response-Suppress: All"},
		{"Auto-Submitted: auto-replied", "Precedence: bulk", "X-Auto-Response-Suppress: All"},
	}
	for i, hdrs := range variants {
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      fmt.Sprintf("OOO Robot %d <%s@example.test>", i, uniq("ooo")),
			subject:   fmt.Sprintf("Out of office (variant %d)", i),
			messageID: uniq("storm") + "@example.test",
			headers:   hdrs,
		}.raw())
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if !res.Suppressed {
			t.Errorf("variant %d (%v) not suppressed", i, hdrs)
		}
		if !res.NewTicket || res.TicketID == uuid.Nil || res.ArticleID == uuid.Nil {
			t.Errorf("variant %d not ingested-but-suppressed: %+v", i, res)
		}
		if n := countTicketArticles(t, res.TicketID); n != 1 {
			t.Errorf("variant %d articles = %d, want 1", i, n)
		}
	}
	if n := len(listJobs(t, AutoAckArgs{}.Kind())); n != 0 {
		t.Errorf("auto-ack jobs after storm = %d, want 0", n)
	}
	if n := len(listJobs(t, NotifyAssigneeArgs{}.Kind())); n != 0 {
		t.Errorf("notify jobs after storm = %d, want 0", n)
	}

	// Against an ASSIGNED ticket: a suppressed reply must not notify the
	// assignee either.
	agent := makeUser(t, store.UserRoleAgent)
	seedID := uniq("storm-seed") + "@example.test"
	seed, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: "Human <" + uniq("human") + "@example.test>", subject: "Real issue", messageID: seedID,
	}.raw())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.New(testPool).UpdateTicketAssignee(ctx, store.UpdateTicketAssigneeParams{
		ID: seed.TicketID, AssigneeID: pgtype.UUID{Bytes: agent.ID, Valid: true},
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "OOO Robot <" + uniq("ooo") + "@example.test>",
		subject:   "Automatic reply: Real issue",
		messageID: uniq("storm-reply") + "@example.test",
		headers:   []string{"In-Reply-To: <" + seedID + ">", "Auto-Submitted: auto-replied"},
	}.raw())
	if err != nil {
		t.Fatalf("suppressed reply: %v", err)
	}
	if !res.Suppressed || res.NewTicket || res.TicketID != seed.TicketID {
		t.Fatalf("suppressed reply result: %+v", res)
	}
	if n := len(listJobs(t, NotifyAssigneeArgs{}.Kind())); n != 0 {
		t.Errorf("notify jobs = %d, want 0 (suppressed reply to assigned ticket)", n)
	}
	// The human seed is the only ack ever enqueued.
	if n := len(listJobs(t, AutoAckArgs{}.Kind())); n != 1 {
		t.Errorf("auto-ack jobs = %d, want 1 (the human seed only)", n)
	}
}

// TestTortureAckBounceNoLoop: our own auto-ack "bounces back" — a fresh
// Message-ID referencing the ack's id, flagged Auto-Submitted. It must
// thread into the ticket, be suppressed, and never produce an ack-of-ack.
func TestTortureAckBounceNoLoop(t *testing.T) {
	ctx := context.Background()
	sink := newSMTPSink(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{smtpPort: sink.port(), autoAck: true})

	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Bouncer <" + uniq("bounce") + "@example.test>",
		subject:   "Please help",
		messageID: uniq("orig") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	ack := sink.waitForMessages(t, 1, 20*time.Second)[0]
	ackID := canonicalMessageID(ack.header("Message-ID"))

	bounce, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Mail Delivery System <" + uniq("mailer-daemon") + "@example.test>",
		subject:   "Undeliverable: " + ack.header("Subject"),
		messageID: uniq("dsn") + "@example.test",
		headers:   []string{"References: <" + ackID + ">", "Auto-Submitted: auto-replied"},
		body:      "delivery failed\n",
	}.raw())
	if err != nil {
		t.Fatalf("ingest bounce: %v", err)
	}
	if !bounce.Suppressed {
		t.Errorf("bounce not suppressed: %+v", bounce)
	}
	if bounce.NewTicket || bounce.TicketID != res.TicketID {
		t.Errorf("bounce did not thread via ack id: %+v (want %s)", bounce, res.TicketID)
	}

	if n := len(listJobs(t, AutoAckArgs{}.Kind())); n != 1 {
		t.Errorf("auto-ack jobs = %d, want 1 (no ack-of-ack)", n)
	}
	if n := len(listJobs(t, EmailSendArgs{}.Kind())); n != 1 {
		t.Errorf("email_send jobs = %d, want 1 (the original ack only)", n)
	}
	// No further mail leaves the building.
	assertSinkStays(t, sink, 1, 1500*time.Millisecond)
	if n := countTicketArticles(t, res.TicketID); n != 3 {
		t.Errorf("articles = %d, want 3 (original, ack, bounce)", n)
	}
}

// ---------------------------------------------------------------------------
// Scenario 6 — subject-token fallback
// ---------------------------------------------------------------------------

// TestTortureSubjectToken: token threading with EMPTY References/
// In-Reply-To; forged token for a nonexistent number; token pointing at
// ticket B while References point at ticket A (References must win).
func TestTortureSubjectToken(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	aID := uniq("tok-a") + "@example.test"
	resA, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: "Tok A <" + uniq("toka") + "@example.test>", subject: "Alpha", messageID: aID,
	}.raw())
	if err != nil {
		t.Fatalf("seed A: %v", err)
	}
	resB, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: "Tok B <" + uniq("tokb") + "@example.test>", subject: "Beta", messageID: uniq("tok-b") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("seed B: %v", err)
	}
	numA := getTicket(t, resA.TicketID).Number
	numB := getTicket(t, resB.TicketID).Number

	t.Run("token-threads-with-stripped-headers", func(t *testing.T) {
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Tok A <" + uniq("toka") + "@example.test>",
			subject:   "my client stripped everything [SD-" + numA + "]",
			messageID: uniq("tok") + "@example.test",
		}.raw())
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if res.NewTicket || res.TicketID != resA.TicketID {
			t.Fatalf("token did not thread: %+v (want %s)", res, resA.TicketID)
		}
	})

	t.Run("forged-token-nonexistent-number", func(t *testing.T) {
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Forger <" + uniq("forge") + "@example.test>",
			subject:   "URGENT [SD-99991231-9999]",
			messageID: uniq("forge") + "@example.test",
		}.raw())
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if !res.NewTicket {
			t.Fatalf("forged token must open a new ticket: %+v", res)
		}
		// The bogus token is stripped from the new ticket's subject.
		if got := getTicket(t, res.TicketID).Subject; got != "URGENT" {
			t.Errorf("subject = %q, want %q (token stripped)", got, "URGENT")
		}
	})

	t.Run("references-beat-token", func(t *testing.T) {
		beforeB := countTicketArticles(t, resB.TicketID)
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Confused <" + uniq("conf") + "@example.test>",
			subject:   "Re: mixed signals [SD-" + numB + "]",
			messageID: uniq("mixed") + "@example.test",
			headers:   []string{"References: <" + aID + ">"},
		}.raw())
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if res.NewTicket || res.TicketID != resA.TicketID {
			t.Fatalf("References must beat the subject token: %+v (want %s)", res, resA.TicketID)
		}
		if n := countTicketArticles(t, resB.TicketID); n != beforeB {
			t.Errorf("ticket B grew (%d -> %d)", beforeB, n)
		}
	})
}

// ---------------------------------------------------------------------------
// Scenario 7 — malice & malformation
// ---------------------------------------------------------------------------

func TestTortureMalformed(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	t.Run("oversized-attachment-dropped-with-note", func(t *testing.T) {
		// ~60MB attachment: over the 50MiB cap. The mail must still ingest
		// — without the attachment, and with a visible internal system note.
		line := strings.Repeat("A", 72) + "\n"
		var body strings.Builder
		body.Grow(66 << 20)
		body.WriteString("--CAPB\nContent-Type: text/plain; charset=utf-8\n\nsee attached log\n")
		body.WriteString("--CAPB\nContent-Type: text/plain; name=\"huge.log\"\n")
		body.WriteString("Content-Disposition: attachment; filename=\"huge.log\"\n\n")
		for range 860_000 { // 860k * 74 bytes on the wire ≈ 60MB decoded
			body.WriteString(line)
		}
		body.WriteString("--CAPB--\n")
		raw := rawMessage([]string{
			"From: Bulk Sender <" + uniq("bulk") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: Log attached",
			"Message-ID: <" + uniq("cap") + "@example.test>",
			"MIME-Version: 1.0",
			`Content-Type: multipart/mixed; boundary="CAPB"`,
		}, body.String())

		res, err := timeBox(t, 90*time.Second, "oversized-attachment ingest", func() (InboundResult, error) {
			return e.ProcessMessage(ctx, mb, raw)
		})
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if !res.NewTicket || res.Suppressed {
			t.Fatalf("result: %+v", res)
		}
		atts, err := store.New(testPool).ListArticleAttachments(ctx, res.ArticleID)
		if err != nil {
			t.Fatalf("list attachments: %v", err)
		}
		if len(atts) != 0 {
			t.Errorf("attachments = %d, want 0 (over cap)", len(atts))
		}
		arts := listTicketArticles(t, res.TicketID)
		if len(arts) != 2 {
			t.Fatalf("articles = %d, want 2 (mail + system note): %+v", len(arts), arts)
		}
		var note *tortureArticle
		for i := range arts {
			if arts[i].SenderType == "system" && arts[i].IsInternal {
				note = &arts[i]
			}
		}
		if note == nil {
			t.Fatalf("no internal system note about the dropped attachment")
		}
		if !strings.Contains(note.BodyText, "huge.log") {
			t.Errorf("note = %q, want it to name huge.log", note.BodyText)
		}
	})

	t.Run("nested-mime-bomb-bounded", func(t *testing.T) {
		// 50 levels of nested multipart. Must terminate quickly, never
		// hang, and still produce exactly one ticket (parsed or stub).
		inner := "Content-Type: text/plain; charset=utf-8\r\n\r\ndeep breath\r\n"
		for i := 50; i > 0; i-- {
			b := fmt.Sprintf("LVL%03d", i)
			inner = "Content-Type: multipart/mixed; boundary=\"" + b + "\"\r\n\r\n" +
				"--" + b + "\r\n" + inner + "--" + b + "--\r\n"
		}
		raw := []byte("From: Bomber <" + uniq("bomb") + "@example.test>\r\n" +
			"To: " + mb.EmailAddress + "\r\n" +
			"Subject: matryoshka\r\n" +
			"Message-ID: <" + uniq("bomb") + "@example.test>\r\n" +
			"MIME-Version: 1.0\r\n" +
			inner)

		res, err := timeBox(t, 20*time.Second, "nested-MIME ingest", func() (InboundResult, error) {
			return e.ProcessMessage(ctx, mb, raw)
		})
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.TicketID == uuid.Nil || res.ArticleID == uuid.Nil {
			t.Fatalf("nested MIME produced no ticket/article: %+v", res)
		}
		if n := countTicketArticles(t, res.TicketID); n != 1 {
			t.Errorf("articles = %d, want 1", n)
		}
	})

	t.Run("html-only-script-sanitized", func(t *testing.T) {
		raw := rawMessage([]string{
			"From: XSS <" + uniq("xss") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: totally legit invoice",
			"Message-ID: <" + uniq("xss") + "@example.test>",
			"MIME-Version: 1.0",
			"Content-Type: text/html; charset=utf-8",
		}, `<html><body><p>Hello support team</p>`+
			`<script>document.location='https://evil.example/steal'</script>`+
			`<img src=x onerror="alert(document.cookie)">`+
			`<a href="javascript:alert(1)">click</a></body></html>`)
		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		a := getArticle(t, res.ArticleID)
		htmlBody := textVal(a.BodyHtml)
		if htmlBody == "" {
			t.Fatalf("body_html empty for HTML-only mail")
		}
		lower := strings.ToLower(htmlBody)
		for _, bad := range []string{"<script", "onerror", "javascript:"} {
			if strings.Contains(lower, bad) {
				t.Errorf("sanitized body_html still contains %q: %q", bad, htmlBody)
			}
		}
		if !strings.Contains(htmlBody, "Hello support team") {
			t.Errorf("sanitization destroyed the content: %q", htmlBody)
		}
		if strings.TrimSpace(a.BodyText) == "" || !strings.Contains(a.BodyText, "Hello support team") {
			t.Errorf("plaintext extraction from HTML-only mail = %q", a.BodyText)
		}
	})

	t.Run("rfc2047-subject-decoded", func(t *testing.T) {
		raw := rawMessage([]string{
			"From: Latin <" + uniq("latin") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: =?ISO-8859-1?Q?St=F6rung_der_Telefonanlage?=",
			"Message-ID: <" + uniq("2047") + "@example.test>",
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=us-ascii",
		}, "phone system down\n")
		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if got := getTicket(t, res.TicketID).Subject; got != "Störung der Telefonanlage" {
			t.Errorf("subject = %q, want decoded RFC 2047", got)
		}
	})

	t.Run("base64-wrong-charset-no-crash", func(t *testing.T) {
		// Latin-1 bytes declared as UTF-8, base64-encoded: must not error,
		// and the stored body must be valid UTF-8 (replacement runes ok).
		latin1 := []byte("Gr\xfc\xdfe aus M\xfcnchen, bitte um R\xfcckruf\n")
		raw := rawMessage([]string{
			"From: Charset Liar <" + uniq("charset") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: encoding soup",
			"Message-ID: <" + uniq("charset") + "@example.test>",
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=utf-8",
			"Content-Transfer-Encoding: base64",
		}, base64.StdEncoding.EncodeToString(latin1)+"\n")
		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("ProcessMessage must tolerate charset lies: %v", err)
		}
		body := getArticle(t, res.ArticleID).BodyText
		if strings.TrimSpace(body) == "" {
			t.Errorf("body empty")
		}
		if !utf8.ValidString(body) {
			t.Errorf("stored body_text is not valid UTF-8: %q", body)
		}
	})

	t.Run("references-10kb-capped-walk", func(t *testing.T) {
		seedID := uniq("refseed") + "@example.test"
		seed, err := e.ProcessMessage(ctx, mb, inboundMail{
			from: "Ref Seed <" + uniq("ref") + "@example.test>", subject: "Ref anchor", messageID: seedID,
		}.raw())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		// ~11KB of junk ancestry. The known id sits at the NEWEST end, so
		// the capped walk still threads.
		var refs strings.Builder
		for i := range 400 {
			fmt.Fprintf(&refs, "<junk-%04d-%s@flood.example> ", i, uniq("j"))
		}
		refs.WriteString("<" + seedID + ">")

		start := time.Now()
		res, err := timeBox(t, 10*time.Second, "10KB References ingest", func() (InboundResult, error) {
			return e.ProcessMessage(ctx, mb, inboundMail{
				from:      "Flood <" + uniq("flood") + "@example.test>",
				subject:   "Re: Ref anchor",
				messageID: uniq("floodmsg") + "@example.test",
				headers:   []string{"References: " + refs.String()},
			}.raw())
		})
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.NewTicket || res.TicketID != seed.TicketID {
			t.Fatalf("giant References did not thread via newest id: %+v", res)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("10KB References took %s (walk not capped?)", elapsed)
		}

		// All-junk variant: must fall through to a new ticket, still fast.
		var junk strings.Builder
		for i := range 400 {
			fmt.Fprintf(&junk, "<junk-%04d-%s@flood.example> ", i, uniq("k"))
		}
		start = time.Now()
		res2, err := timeBox(t, 10*time.Second, "all-junk References ingest", func() (InboundResult, error) {
			return e.ProcessMessage(ctx, mb, inboundMail{
				from:      "Flood <" + uniq("flood") + "@example.test>",
				subject:   "unthreadable flood",
				messageID: uniq("floodmsg") + "@example.test",
				headers:   []string{"References: " + strings.TrimSpace(junk.String())},
			}.raw())
		})
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if !res2.NewTicket {
			t.Fatalf("all-junk References should open a new ticket: %+v", res2)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("all-junk References took %s (walk not capped?)", elapsed)
		}
	})

	t.Run("duplicate-message-id-headers-first-wins", func(t *testing.T) {
		firstID := uniq("dupfirst") + "@example.test"
		secondID := uniq("dupsecond") + "@example.test"
		raw := rawMessage([]string{
			"From: Twins <" + uniq("twin") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: two message ids",
			"Message-ID: <" + firstID + ">",
			"Message-ID: <" + secondID + ">",
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=utf-8",
		}, "which id am I?\n")

		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if got := textVal(getArticle(t, res.ArticleID).MessageID); got != firstID {
			t.Errorf("stored message_id = %q, want the FIRST header %q", got, firstID)
		}
		if _, err := store.New(testPool).GetEmailMessageID(ctx, firstID); err != nil {
			t.Errorf("first id not in email_message_ids: %v", err)
		}
		if _, err := store.New(testPool).GetEmailMessageID(ctx, secondID); err == nil {
			t.Errorf("second Message-ID header was recorded; first must win deterministically")
		}
		// Deterministic across redelivery: identical bytes dedup.
		again, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if !again.Duplicate {
			t.Errorf("redelivery with duplicate Message-ID headers not deduped: %+v", again)
		}
	})
}

// ---------------------------------------------------------------------------
// Scenario 8 — sender edge cases
// ---------------------------------------------------------------------------

func TestTortureSenderEdges(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	clearJobs(t)
	mb := makeMailbox(t, mailboxOpts{autoAck: true})

	t.Run("admin-from-no-privilege-side-effects", func(t *testing.T) {
		admin := makeUser(t, store.UserRoleAdmin)
		custFrom := "Cust <" + uniq("cust") + "@example.test>"
		seedID := uniq("adm-seed") + "@example.test"
		seed, err := e.ProcessMessage(ctx, mb, inboundMail{
			from: custFrom, subject: "Admin will answer", messageID: seedID,
		}.raw())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		origRequester := getTicket(t, seed.TicketID).RequesterID

		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "The Boss <" + admin.Email + ">",
			subject:   "Re: Admin will answer",
			messageID: uniq("adm") + "@example.test",
			headers:   []string{"In-Reply-To: <" + seedID + ">"},
			body:      "I'll handle this one.\n",
		}.raw())
		if err != nil {
			t.Fatalf("admin reply: %v", err)
		}
		if res.NewTicket || res.TicketID != seed.TicketID {
			t.Fatalf("admin reply did not thread: %+v", res)
		}

		a := getArticle(t, res.ArticleID)
		if a.SenderType != store.ArticleSenderAgent {
			t.Errorf("sender_type = %q, want agent (admin authors as staff)", a.SenderType)
		}
		if uuid.UUID(a.AuthorID.Bytes) != admin.ID {
			t.Errorf("author = %v, want admin %s", a.AuthorID, admin.ID)
		}
		// No privilege side effects: role/active/token_version untouched,
		// no second user row for the address.
		u, err := store.New(testPool).GetUserByEmail(ctx, admin.Email)
		if err != nil {
			t.Fatalf("reload admin: %v", err)
		}
		if u.ID != admin.ID || u.Role != store.UserRoleAdmin || u.Active != admin.Active || u.TokenVersion != admin.TokenVersion || u.Name != admin.Name {
			t.Errorf("admin row mutated by inbound mail: %+v vs %+v", u, admin)
		}
		var dupes int
		if err := testPool.QueryRow(ctx, "SELECT count(*) FROM users WHERE email = $1", admin.Email).Scan(&dupes); err != nil {
			t.Fatalf("count users: %v", err)
		}
		if dupes != 1 {
			t.Errorf("users with admin email = %d, want 1", dupes)
		}
		// Requester stays the original customer.
		if got := getTicket(t, seed.TicketID).RequesterID; got != origRequester {
			t.Errorf("requester changed %s -> %s", origRequester, got)
		}
		// Agent-from mail never flips status.
		if got := getTicket(t, seed.TicketID).Status; got != store.TicketStatusOpen {
			t.Errorf("status = %q, want open (unchanged)", got)
		}
	})

	t.Run("self-loop-from-own-address-suppressed", func(t *testing.T) {
		before := len(listJobs(t, AutoAckArgs{}.Kind()))
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Ourselves <" + mb.EmailAddress + ">",
			subject:   "I am mailing myself",
			messageID: uniq("selfloop") + "@example.test",
		}.raw())
		if err != nil {
			t.Fatalf("self-loop ingest: %v", err)
		}
		if !res.Suppressed {
			t.Fatalf("mail From the mailbox's own address must be suppressed: %+v", res)
		}
		if res.TicketID == uuid.Nil {
			t.Fatalf("self-loop mail should still ingest for visibility: %+v", res)
		}
		if after := len(listJobs(t, AutoAckArgs{}.Kind())); after != before {
			t.Errorf("self-loop enqueued an auto-ack (%d -> %d): the mailbox would ack itself forever", before, after)
		}
	})

	t.Run("missing-from-synthesized-customer", func(t *testing.T) {
		raw := rawMessage([]string{
			"To: " + mb.EmailAddress,
			"Subject: who am I",
			"Message-ID: <" + uniq("nofrom") + "@example.test>",
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=utf-8",
		}, "anonymous plea for help\n")
		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("missing From must not error: %v", err)
		}
		if !res.NewTicket {
			t.Fatalf("result: %+v", res)
		}
		u := requester(t, res.TicketID)
		if u.Email != "unknown-sender@invalid" {
			t.Errorf("requester email = %q, want unknown-sender@invalid", u.Email)
		}
		if u.Role != store.UserRoleCustomer {
			t.Errorf("synthesized sender role = %q, want customer", u.Role)
		}
	})
}

// ---------------------------------------------------------------------------
// Scenario 9 — closed-ticket reopen matrix
// ---------------------------------------------------------------------------

// TestTortureClosedReopenMatrix: a human customer reply to a closed ticket
// reopens it (+ status event); a loop-guarded (suppressed) reply adds its
// article but must NOT reopen — machine mail cannot drive workflow.
func TestTortureClosedReopenMatrix(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})
	sdb := store.New(testPool)

	t.Run("customer-reply-reopens", func(t *testing.T) {
		from := "Reopener <" + uniq("re") + "@example.test>"
		seedID := uniq("re-seed") + "@example.test"
		seed, err := e.ProcessMessage(ctx, mb, inboundMail{
			from: from, subject: "Broke once", messageID: seedID,
		}.raw())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := sdb.UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
			ID: seed.TicketID, Status: store.TicketStatusClosed,
		}); err != nil {
			t.Fatalf("close: %v", err)
		}
		eventsBefore := countEvents(t, seed.TicketID, ticket.EventStatusChanged)

		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from: from, subject: "it broke again", messageID: uniq("re-again") + "@example.test",
			headers: []string{"In-Reply-To: <" + seedID + ">"},
		}.raw())
		if err != nil {
			t.Fatalf("reply: %v", err)
		}
		if res.NewTicket || res.TicketID != seed.TicketID {
			t.Fatalf("reply did not thread: %+v", res)
		}
		tk := getTicket(t, seed.TicketID)
		if tk.Status != store.TicketStatusOpen {
			t.Errorf("status = %q, want open (reopened)", tk.Status)
		}
		if tk.ClosedAt.Valid {
			t.Errorf("closed_at still set after reopen")
		}
		if got := countEvents(t, seed.TicketID, ticket.EventStatusChanged); got != eventsBefore+1 {
			t.Errorf("status_changed events = %d, want %d (reopen must be audited)", got, eventsBefore+1)
		}
	})

	t.Run("suppressed-reply-does-not-reopen", func(t *testing.T) {
		from := "Quiet Customer <" + uniq("qc") + "@example.test>"
		seedID := uniq("sup-seed") + "@example.test"
		seed, err := e.ProcessMessage(ctx, mb, inboundMail{
			from: from, subject: "Solved and closed", messageID: seedID,
		}.raw())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := sdb.UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
			ID: seed.TicketID, Status: store.TicketStatusClosed,
		}); err != nil {
			t.Fatalf("close: %v", err)
		}
		eventsBefore := countEvents(t, seed.TicketID, ticket.EventStatusChanged)

		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "OOO Bot <" + uniq("ooo") + "@example.test>",
			subject:   "Automatic reply: Solved and closed",
			messageID: uniq("sup-reply") + "@example.test",
			headers:   []string{"In-Reply-To: <" + seedID + ">", "Auto-Submitted: auto-replied"},
			body:      "I will be back never.\n",
		}.raw())
		if err != nil {
			t.Fatalf("suppressed reply: %v", err)
		}
		if !res.Suppressed || res.NewTicket || res.TicketID != seed.TicketID {
			t.Fatalf("suppressed reply result: %+v", res)
		}
		// The article IS added (visibility)…
		if n := countTicketArticles(t, seed.TicketID); n != 2 {
			t.Errorf("articles = %d, want 2", n)
		}
		// …but the ticket stays closed, closed_at intact, no status event.
		tk := getTicket(t, seed.TicketID)
		if tk.Status != store.TicketStatusClosed {
			t.Errorf("status = %q, want closed (loop-guarded mail must not reopen)", tk.Status)
		}
		if !tk.ClosedAt.Valid {
			t.Errorf("closed_at cleared by a suppressed reply")
		}
		if got := countEvents(t, seed.TicketID, ticket.EventStatusChanged); got != eventsBefore {
			t.Errorf("status_changed events = %d, want %d (no phantom transitions)", got, eventsBefore)
		}
	})

	t.Run("suppressed-reply-does-not-flip-waiting", func(t *testing.T) {
		from := "Waiting Customer <" + uniq("wc") + "@example.test>"
		seedID := uniq("wait-seed") + "@example.test"
		seed, err := e.ProcessMessage(ctx, mb, inboundMail{
			from: from, subject: "Waiting on customer", messageID: seedID,
		}.raw())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := sdb.UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
			ID: seed.TicketID, Status: store.TicketStatusWaitingOnCustomer,
		}); err != nil {
			t.Fatalf("set waiting: %v", err)
		}
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "OOO Bot <" + uniq("ooo") + "@example.test>",
			subject:   "Automatic reply: Waiting on customer",
			messageID: uniq("wait-reply") + "@example.test",
			headers:   []string{"In-Reply-To: <" + seedID + ">", "Precedence: bulk"},
		}.raw())
		if err != nil {
			t.Fatalf("suppressed reply: %v", err)
		}
		if !res.Suppressed {
			t.Fatalf("not suppressed: %+v", res)
		}
		if got := getTicket(t, seed.TicketID).Status; got != store.TicketStatusWaitingOnCustomer {
			t.Errorf("status = %q, want waiting_on_customer (machine mail is not the customer answering)", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Scenario 10 — two mailboxes
// ---------------------------------------------------------------------------

// TestTortureTwoMailboxes: the same customer mails two mailboxes — two
// tickets; a reply to ticket-1's ack threads to ticket 1 only; outbound
// mail for each ticket leaves through its own mailbox's SMTP identity.
func TestTortureTwoMailboxes(t *testing.T) {
	ctx := context.Background()
	sink1 := newSMTPSink(t)
	sink2 := newSMTPSink(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)

	mb1 := makeMailbox(t, mailboxOpts{
		address: uniq("desk-one") + "@slatedesk.test", smtpPort: sink1.port(), autoAck: true, displayName: "Desk One",
	})
	mb2 := makeMailbox(t, mailboxOpts{
		address: uniq("desk-two") + "@slatedesk.test", smtpPort: sink2.port(), autoAck: true, displayName: "Desk Two",
	})

	custAddr := uniq("multi") + "@example.test"
	from := "Multi Customer <" + custAddr + ">"

	res1, err := e.ProcessMessage(ctx, mb1, inboundMail{
		from: from, subject: "Issue for desk one", messageID: uniq("m1") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("mail to mb1: %v", err)
	}
	res2, err := e.ProcessMessage(ctx, mb2, inboundMail{
		from: from, subject: "Different issue for desk two", messageID: uniq("m2") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("mail to mb2: %v", err)
	}
	if !res1.NewTicket || !res2.NewTicket || res1.TicketID == res2.TicketID {
		t.Fatalf("expected two distinct tickets: %+v / %+v", res1, res2)
	}
	// Same customer row backs both tickets.
	if r1, r2 := requester(t, res1.TicketID), requester(t, res2.TicketID); r1.ID != r2.ID || r1.Email != custAddr {
		t.Errorf("requesters differ: %s vs %s", r1.ID, r2.ID)
	}

	// Each ack leaves through its own mailbox identity.
	ack1 := sink1.waitForMessages(t, 1, 20*time.Second)[0]
	ack2 := sink2.waitForMessages(t, 1, 20*time.Second)[0]
	if ack1.From != mb1.EmailAddress {
		t.Errorf("ack1 envelope from = %q, want %q", ack1.From, mb1.EmailAddress)
	}
	if ack2.From != mb2.EmailAddress {
		t.Errorf("ack2 envelope from = %q, want %q", ack2.From, mb2.EmailAddress)
	}
	if !strings.Contains(ack1.header("From"), mb1.EmailAddress) || !strings.Contains(ack1.header("Subject"), "[SD-"+getTicket(t, res1.TicketID).Number+"]") {
		t.Errorf("ack1 identity/subject wrong: From=%q Subject=%q", ack1.header("From"), ack1.header("Subject"))
	}
	if !strings.Contains(ack2.header("From"), mb2.EmailAddress) {
		t.Errorf("ack2 identity wrong: %q", ack2.header("From"))
	}
	ack1ID := canonicalMessageID(ack1.header("Message-ID"))
	assertOutboundRecorded(t, ack1ID, res1.TicketID)
	assertOutboundRecorded(t, canonicalMessageID(ack2.header("Message-ID")), res2.TicketID)

	// The customer replies to TICKET 1's ack while ticket 2 is open: it
	// must thread to ticket 1 only.
	before2 := countTicketArticles(t, res2.TicketID)
	resReply, err := e.ProcessMessage(ctx, mb1, inboundMail{
		from: from, subject: "Re: " + ack1.header("Subject"),
		messageID: uniq("m1-reply") + "@example.test",
		headers:   []string{"In-Reply-To: <" + ack1ID + ">", "References: <" + ack1ID + ">"},
	}.raw())
	if err != nil {
		t.Fatalf("reply to ack1: %v", err)
	}
	if resReply.NewTicket || resReply.TicketID != res1.TicketID {
		t.Fatalf("reply to ticket-1 ack landed on %v, want %s", resReply, res1.TicketID)
	}
	if n := countTicketArticles(t, res2.TicketID); n != before2 {
		t.Errorf("ticket 2 grew from ticket-1 traffic (%d -> %d)", before2, n)
	}

	// Agent replies on both tickets: each goes out via its own mailbox.
	agent := makeUser(t, store.UserRoleAgent)
	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	if _, err := svc.AddArticle(ctx, res1.TicketID, ticket.ArticleInput{
		AuthorID: &agent.ID, SenderType: store.ArticleSenderAgent,
		Channel: store.ArticleChannelWeb, BodyText: "reply on desk one",
	}); err != nil {
		t.Fatalf("reply ticket1: %v", err)
	}
	if _, err := svc.AddArticle(ctx, res2.TicketID, ticket.ArticleInput{
		AuthorID: &agent.ID, SenderType: store.ArticleSenderAgent,
		Channel: store.ArticleChannelWeb, BodyText: "reply on desk two",
	}); err != nil {
		t.Fatalf("reply ticket2: %v", err)
	}
	msgs1 := sink1.waitForMessages(t, 2, 20*time.Second)
	msgs2 := sink2.waitForMessages(t, 2, 20*time.Second)
	for _, m := range msgs1 {
		if m.From != mb1.EmailAddress {
			t.Errorf("sink1 saw mail from %q, want only %q", m.From, mb1.EmailAddress)
		}
	}
	for _, m := range msgs2 {
		if m.From != mb2.EmailAddress {
			t.Errorf("sink2 saw mail from %q, want only %q", m.From, mb2.EmailAddress)
		}
	}
	if !strings.Contains(msgs1[1].Data, "reply on desk one") {
		t.Errorf("sink1 second message is not ticket-1's agent reply")
	}
	if !strings.Contains(msgs2[1].Data, "reply on desk two") {
		t.Errorf("sink2 second message is not ticket-2's agent reply")
	}
	// No cross-talk: each sink saw exactly its own two messages.
	assertSinkStays(t, sink1, 2, 750*time.Millisecond)
	assertSinkStays(t, sink2, 2, 250*time.Millisecond)
}
