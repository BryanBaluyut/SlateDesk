package email

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// inboundMail is a convenience builder for test messages.
type inboundMail struct {
	from      string
	subject   string
	messageID string
	headers   []string
	body      string
}

func (m inboundMail) raw() []byte {
	headers := []string{
		"From: " + m.from,
		"To: support@slatedesk.test",
		"Subject: " + m.subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}
	if m.messageID != "" {
		headers = append(headers, "Message-ID: <"+m.messageID+">")
	}
	headers = append(headers, m.headers...)
	body := m.body
	if body == "" {
		body = "Hello, I need help.\n"
	}
	return rawMessage(headers, body)
}

// TestInboundHappyPath: full new-ticket ingest — ticket + article +
// attachment (blob and row) + message-id row + auto-ack enqueue, all
// observable after ProcessMessage returns.
func TestInboundHappyPath(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	clearJobs(t)
	mb := makeMailbox(t, mailboxOpts{autoAck: true})

	from := uniq("alice") + "@example.test"
	msgID := uniq("msg") + "@example.test"
	raw := rawMessage([]string{
		"From: Alice Example <" + from + ">",
		"To: " + mb.EmailAddress,
		"Subject: Re: Fwd: Printer on fire",
		"Message-ID: <" + msgID + ">",
		"MIME-Version: 1.0",
		`Content-Type: multipart/mixed; boundary="BNDRY"`,
	}, `--BNDRY
Content-Type: text/plain; charset=utf-8

Help, the printer is on fire!
--BNDRY
Content-Type: text/plain; name="notes.txt"
Content-Disposition: attachment; filename="notes.txt"

it is very much on fire
--BNDRY--
`)

	res, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if res.Duplicate || res.Suppressed || !res.NewTicket {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Ticket: subject stripped of Re:/Fwd:, open, requester auto-created
	// as customer.
	tk := getTicket(t, res.TicketID)
	if tk.Subject != "Printer on fire" {
		t.Errorf("subject = %q, want %q", tk.Subject, "Printer on fire")
	}
	if tk.Status != store.TicketStatusOpen {
		t.Errorf("status = %q, want open", tk.Status)
	}
	requester, err := store.New(testPool).GetUserByID(ctx, tk.RequesterID)
	if err != nil {
		t.Fatalf("load requester: %v", err)
	}
	if requester.Email != from || requester.Role != store.UserRoleCustomer || requester.Name != "Alice Example" {
		t.Errorf("requester = %s/%s/%s, want %s/customer/Alice Example", requester.Email, requester.Role, requester.Name, from)
	}

	// Article: body from the text part, email channel, headers stamped.
	a := getArticle(t, res.ArticleID)
	if !strings.Contains(a.BodyText, "printer is on fire") {
		t.Errorf("body_text = %q", a.BodyText)
	}
	if a.Channel != store.ArticleChannelEmail || a.SenderType != store.ArticleSenderCustomer || a.IsInternal {
		t.Errorf("article channel/sender/internal = %v/%v/%v", a.Channel, a.SenderType, a.IsInternal)
	}
	if textVal(a.MessageID) != msgID {
		t.Errorf("article message_id = %q, want %q", textVal(a.MessageID), msgID)
	}

	// Message-ID row (dedup + threading table), inbound direction.
	row, err := store.New(testPool).GetEmailMessageID(ctx, msgID)
	if err != nil {
		t.Fatalf("message id row: %v", err)
	}
	if row.Direction != store.EmailDirectionInbound || row.TicketID != tk.ID {
		t.Errorf("message id row = %+v", row)
	}

	// Attachment row + blob round-trip.
	atts, err := store.New(testPool).ListArticleAttachments(ctx, a.ID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(atts) != 1 || atts[0].Filename != "notes.txt" {
		t.Fatalf("attachments = %+v, want one notes.txt", atts)
	}
	rc, err := e.blobs.Open(ctx, atts[0].StorageKey)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	blob, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(blob), "very much on fire") {
		t.Errorf("blob = %q", blob)
	}

	// Auto-ack job enqueued in the same transaction.
	acks := listJobs(t, AutoAckArgs{}.Kind())
	if len(acks) != 1 {
		t.Fatalf("auto-ack jobs = %d, want 1", len(acks))
	}
	if acks[0].Args["ticket_id"] != tk.ID.String() {
		t.Errorf("ack ticket_id = %v, want %s", acks[0].Args["ticket_id"], tk.ID)
	}
}

// TestInboundDedupRedelivery: the same Message-ID delivered twice ingests
// exactly once (crash-before-\Seen / IMAP redelivery case).
func TestInboundDedupRedelivery(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	m := inboundMail{
		from:      "Bob <" + uniq("bob") + "@example.test>",
		subject:   "Dedup me",
		messageID: uniq("dup") + "@example.test",
	}

	first, err := e.ProcessMessage(ctx, mb, m.raw())
	if err != nil {
		t.Fatalf("first ProcessMessage: %v", err)
	}
	second, err := e.ProcessMessage(ctx, mb, m.raw())
	if err != nil {
		t.Fatalf("second ProcessMessage: %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("second delivery not flagged duplicate: %+v", second)
	}

	var n int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM articles WHERE ticket_id = $1", first.TicketID).Scan(&n); err != nil {
		t.Fatalf("count articles: %v", err)
	}
	if n != 1 {
		t.Errorf("articles = %d, want 1 (redelivery must not duplicate)", n)
	}
}

// TestInboundLoopGuard: autoresponder markers suppress auto-ack/notify but
// the mail still ingests (visibility).
func TestInboundLoopGuard(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"auto-submitted", "Auto-Submitted: auto-replied"},
		{"precedence-bulk", "Precedence: bulk"},
		{"precedence-auto-reply", "Precedence: auto_reply"},
		{"precedence-list", "Precedence: list"},
		{"x-auto-response-suppress", "X-Auto-Response-Suppress: All"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newTestEngine(t)
			wireInsertOnlyRiver(t, e)
			clearJobs(t)
			mb := makeMailbox(t, mailboxOpts{autoAck: true})

			res, err := e.ProcessMessage(ctx, mb, inboundMail{
				from:      "Robot <" + uniq("robot") + "@example.test>",
				subject:   "Out of office",
				messageID: uniq("ooo") + "@example.test",
				headers:   []string{tc.header},
			}.raw())
			if err != nil {
				t.Fatalf("ProcessMessage: %v", err)
			}
			if !res.Suppressed {
				t.Fatalf("not suppressed: %+v", res)
			}
			// Still ingested: ticket + article exist.
			if res.TicketID == uuid.Nil || res.ArticleID == uuid.Nil {
				t.Fatalf("suppressed mail was not ingested: %+v", res)
			}
			// But no jobs.
			if n := len(listJobs(t, AutoAckArgs{}.Kind())); n != 0 {
				t.Errorf("auto-ack jobs = %d, want 0", n)
			}
			if n := len(listJobs(t, NotifyAssigneeArgs{}.Kind())); n != 0 {
				t.Errorf("notify jobs = %d, want 0", n)
			}
		})
	}

	// Auto-Submitted: no is NOT an autoresponder.
	t.Run("auto-submitted-no", func(t *testing.T) {
		e := newTestEngine(t)
		mb := makeMailbox(t, mailboxOpts{})
		res, err := e.ProcessMessage(context.Background(), mb, inboundMail{
			from:      "Human <" + uniq("human") + "@example.test>",
			subject:   "Real mail",
			messageID: uniq("real") + "@example.test",
			headers:   []string{"Auto-Submitted: no"},
		}.raw())
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.Suppressed {
			t.Errorf("Auto-Submitted: no must not suppress")
		}
	})
}

// TestInboundThreading covers all four resolution paths and the new-ticket
// fallback.
func TestInboundThreading(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	seed := func(t *testing.T) (uuid.UUID, string) {
		t.Helper()
		msgID := uniq("seed") + "@example.test"
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Carol <" + uniq("carol") + "@example.test>",
			subject:   "Seed ticket",
			messageID: msgID,
		}.raw())
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		return res.TicketID, msgID
	}

	t.Run("references-right-to-left", func(t *testing.T) {
		ticketID, seedID := seed(t)
		// Rightmost (newest) id is unknown; the walk continues leftward
		// and hits the seed id.
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Carol <" + uniq("carol") + "@example.test>",
			subject:   "Re: Seed ticket",
			messageID: uniq("reply") + "@example.test",
			headers:   []string{"References: <" + seedID + "> <unknown-" + uniq("u") + "@else.test>"},
		}.raw())
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.NewTicket || res.TicketID != ticketID {
			t.Fatalf("did not thread via References: %+v (want ticket %s)", res, ticketID)
		}
	})

	t.Run("in-reply-to", func(t *testing.T) {
		ticketID, seedID := seed(t)
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Carol <" + uniq("carol") + "@example.test>",
			subject:   "no subject relation at all",
			messageID: uniq("reply") + "@example.test",
			headers:   []string{"In-Reply-To: <" + seedID + ">"},
		}.raw())
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.NewTicket || res.TicketID != ticketID {
			t.Fatalf("did not thread via In-Reply-To: %+v", res)
		}
	})

	t.Run("reply-to-our-outbound-id", func(t *testing.T) {
		// v1's broken case: the customer replies to OUR mail. Record an
		// outbound Message-ID for the ticket, then answer it.
		ticketID, _ := seed(t)
		ourID := uniq("outbound") + "@" + mailboxDomain(mb)
		if _, err := store.New(testPool).InsertEmailMessageID(ctx, store.InsertEmailMessageIDParams{
			MessageID: ourID,
			Direction: store.EmailDirectionOutbound,
			MailboxID: pgtype.UUID{Bytes: mb.ID, Valid: true},
			TicketID:  ticketID,
		}); err != nil {
			t.Fatalf("record outbound id: %v", err)
		}
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Carol <" + uniq("carol") + "@example.test>",
			subject:   "Re: your answer",
			messageID: uniq("reply") + "@example.test",
			headers:   []string{"In-Reply-To: <" + ourID + ">"},
		}.raw())
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.NewTicket || res.TicketID != ticketID {
			t.Fatalf("reply to our outbound id did not thread: %+v", res)
		}
	})

	t.Run("subject-token-fallback", func(t *testing.T) {
		ticketID, _ := seed(t)
		number := getTicket(t, ticketID).Number
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Carol <" + uniq("carol") + "@example.test>",
			subject:   "totally stripped headers [SD-" + number + "]",
			messageID: uniq("reply") + "@example.test",
		}.raw())
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if res.NewTicket || res.TicketID != ticketID {
			t.Fatalf("subject token did not thread: %+v", res)
		}
	})

	t.Run("no-match-new-ticket", func(t *testing.T) {
		res, err := e.ProcessMessage(ctx, mb, inboundMail{
			from:      "Dave <" + uniq("dave") + "@example.test>",
			subject:   "Re: something unrelated",
			messageID: uniq("new") + "@example.test",
			headers:   []string{"References: <never-seen-" + uniq("x") + "@else.test>"},
		}.raw())
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if !res.NewTicket {
			t.Fatalf("expected new ticket: %+v", res)
		}
	})
}

// TestInboundCharsets: ISO-8859-1 quoted-printable and UTF-8 base64 decode
// into clean UTF-8 body/subject.
func TestInboundCharsets(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	t.Run("iso-8859-1-quoted-printable", func(t *testing.T) {
		raw := rawMessage([]string{
			"From: Kunde <" + uniq("kunde") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: =?iso-8859-1?Q?St=F6rung_beim_Drucker?=",
			"Message-ID: <" + uniq("latin1") + "@example.test>",
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=iso-8859-1",
			"Content-Transfer-Encoding: quoted-printable",
		}, "Der Drucker zeigt st=E4ndig Fehler f=FCr alle Auftr=E4ge.\n")
		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if got := getTicket(t, res.TicketID).Subject; got != "Störung beim Drucker" {
			t.Errorf("subject = %q", got)
		}
		if body := getArticle(t, res.ArticleID).BodyText; !strings.Contains(body, "ständig Fehler für alle Aufträge") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("utf-8-base64", func(t *testing.T) {
		raw := rawMessage([]string{
			"From: =?utf-8?B?WsO8cmljaA==?= <" + uniq("zurich") + "@example.test>",
			"To: " + mb.EmailAddress,
			"Subject: =?utf-8?B?R3LDvMOfZSAhIQ==?=",
			"Message-ID: <" + uniq("utf8") + "@example.test>",
			"MIME-Version: 1.0",
			"Content-Type: text/plain; charset=utf-8",
			"Content-Transfer-Encoding: base64",
		}, "R3LDvMOfZSBhdXMgWsO8cmljaCEg5L2g5aW9\n")
		res, err := e.ProcessMessage(ctx, mb, raw)
		if err != nil {
			t.Fatalf("ProcessMessage: %v", err)
		}
		if got := getTicket(t, res.TicketID).Subject; got != "Grüße !!" {
			t.Errorf("subject = %q", got)
		}
		if body := getArticle(t, res.ArticleID).BodyText; !strings.Contains(body, "Grüße aus Zürich! 你好") {
			t.Errorf("body = %q", body)
		}
	})
}

// TestInboundAgentFrom: mail From an existing agent's address ingests as
// that agent (sender_type=agent), never auto-creates/promotes, and does
// NOT flip ticket status.
func TestInboundAgentFrom(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})
	agent := makeUser(t, store.UserRoleAgent)

	// Seed a customer ticket, put it in waiting_on_customer via an
	// explicit update (simplest fixture).
	seedRes, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Eve <" + uniq("eve") + "@example.test>",
		subject:   "Need help",
		messageID: uniq("seed") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.New(testPool).UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
		ID: seedRes.TicketID, Status: store.TicketStatusWaitingOnCustomer,
	}); err != nil {
		t.Fatalf("set waiting: %v", err)
	}

	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      agent.Name + " <" + agent.Email + ">",
		subject:   "Re: Need help",
		messageID: uniq("agentreply") + "@example.test",
		headers:   []string{"In-Reply-To: <" + uniq("nothere") + "@example.test>", "References: <" + firstMessageID(t, seedRes.TicketID) + ">"},
	}.raw())
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	if res.NewTicket || res.TicketID != seedRes.TicketID {
		t.Fatalf("agent reply did not thread: %+v", res)
	}

	a := getArticle(t, res.ArticleID)
	if a.SenderType != store.ArticleSenderAgent {
		t.Errorf("sender_type = %q, want agent", a.SenderType)
	}
	if a.IsInternal {
		t.Errorf("agent email reply must not be internal")
	}
	if uuid.UUID(a.AuthorID.Bytes) != agent.ID {
		t.Errorf("author = %v, want %s", a.AuthorID, agent.ID)
	}
	// Role untouched; no duplicate user created.
	u, err := store.New(testPool).GetUserByEmail(ctx, agent.Email)
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	if u.Role != store.UserRoleAgent || u.ID != agent.ID {
		t.Errorf("agent mutated: %+v", u)
	}
	// Status unchanged: agent-from mail never flips (documented in
	// inbound.go — status is driven from the workspace composer).
	if got := getTicket(t, seedRes.TicketID).Status; got != store.TicketStatusWaitingOnCustomer {
		t.Errorf("status = %q, want waiting_on_customer (unchanged)", got)
	}
}

// TestInboundClosedReopen: a customer reply to a closed ticket reopens it
// (open + closed_at cleared + status event).
func TestInboundClosedReopen(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	from := "Frank <" + uniq("frank") + "@example.test>"
	seedRes, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "Broken again", messageID: uniq("seed") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.New(testPool).UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
		ID: seedRes.TicketID, Status: store.TicketStatusClosed,
	}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !getTicket(t, seedRes.TicketID).ClosedAt.Valid {
		t.Fatalf("fixture: closed_at not set")
	}

	res, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "it broke AGAIN", messageID: uniq("reopen") + "@example.test",
		headers: []string{"In-Reply-To: <" + firstMessageID(t, seedRes.TicketID) + ">"},
	}.raw())
	if err != nil {
		t.Fatalf("reopen ProcessMessage: %v", err)
	}
	if res.NewTicket || res.TicketID != seedRes.TicketID {
		t.Fatalf("reopen did not thread: %+v", res)
	}

	tk := getTicket(t, seedRes.TicketID)
	if tk.Status != store.TicketStatusOpen {
		t.Errorf("status = %q, want open", tk.Status)
	}
	if tk.ClosedAt.Valid {
		t.Errorf("closed_at still set after reopen")
	}
	assertHasEvent(t, seedRes.TicketID, "status_changed")
}

// TestInboundWaitingFlipsOpen: customer reply on waiting_on_customer flips
// to open (the M2 matrix applied on the email path).
func TestInboundWaitingFlipsOpen(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	from := "Grace <" + uniq("grace") + "@example.test>"
	seedRes, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "Slow laptop", messageID: uniq("seed") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.New(testPool).UpdateTicketStatus(ctx, store.UpdateTicketStatusParams{
		ID: seedRes.TicketID, Status: store.TicketStatusWaitingOnCustomer,
	}); err != nil {
		t.Fatalf("set waiting: %v", err)
	}

	if _, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "more info", messageID: uniq("more") + "@example.test",
		headers: []string{"In-Reply-To: <" + firstMessageID(t, seedRes.TicketID) + ">"},
	}.raw()); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got := getTicket(t, seedRes.TicketID).Status; got != store.TicketStatusOpen {
		t.Errorf("status = %q, want open", got)
	}
}

// TestInboundNotifyAssigneeOnCustomerReply: a customer reply to an
// assigned ticket enqueues the notify-assignee job in the ingest tx.
func TestInboundNotifyAssigneeOnCustomerReply(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	clearJobs(t)
	mb := makeMailbox(t, mailboxOpts{})
	agent := makeUser(t, store.UserRoleAgent)

	from := "Heidi <" + uniq("heidi") + "@example.test>"
	seedRes, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "VPN down", messageID: uniq("seed") + "@example.test",
	}.raw())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.New(testPool).UpdateTicketAssignee(ctx, store.UpdateTicketAssigneeParams{
		ID: seedRes.TicketID, AssigneeID: pgtype.UUID{Bytes: agent.ID, Valid: true},
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	if _, err := e.ProcessMessage(ctx, mb, inboundMail{
		from: from, subject: "still down", messageID: uniq("reply") + "@example.test",
		headers: []string{"In-Reply-To: <" + firstMessageID(t, seedRes.TicketID) + ">"},
	}.raw()); err != nil {
		t.Fatalf("reply: %v", err)
	}

	notifies := listJobs(t, NotifyAssigneeArgs{}.Kind())
	if len(notifies) != 1 {
		t.Fatalf("notify jobs = %d, want 1", len(notifies))
	}
	if notifies[0].Args["assignee_id"] != agent.ID.String() || notifies[0].Args["reason"] != NotifyReasonCustomerReply {
		t.Errorf("notify args = %+v", notifies[0].Args)
	}
}

// TestInboundSynthesizedMessageID: mail without a Message-ID gets a
// content-hash id, and an identical redelivery still dedups on it.
func TestInboundSynthesizedMessageID(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	raw := inboundMail{
		from:    "Ivan <" + uniq("ivan") + "@example.test>",
		subject: "No message id here",
	}.raw()

	res, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("ProcessMessage: %v", err)
	}
	a := getArticle(t, res.ArticleID)
	if !strings.HasSuffix(textVal(a.MessageID), "@synthesized.slatedesk.invalid") {
		t.Errorf("synthesized id = %q", textVal(a.MessageID))
	}

	again, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !again.Duplicate {
		t.Errorf("identical bytes without Message-ID must dedup: %+v", again)
	}
}

// firstMessageID returns the earliest recorded message id for a ticket.
func firstMessageID(t *testing.T, ticketID uuid.UUID) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(),
		"SELECT message_id FROM email_message_ids WHERE ticket_id = $1 ORDER BY created_at, message_id LIMIT 1",
		ticketID).Scan(&id); err != nil {
		t.Fatalf("first message id: %v", err)
	}
	return id
}

// assertHasEvent fails unless the ticket has at least one event of type
// typ.
func assertHasEvent(t *testing.T, ticketID uuid.UUID, typ string) {
	t.Helper()
	evs, err := store.New(testPool).ListTicketEvents(context.Background(), ticketID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range evs {
		if ev.Type == typ {
			return
		}
	}
	t.Errorf("ticket %s has no %q event (events: %s)", ticketID, typ, eventTypes(evs))
}

func eventTypes(evs []store.TicketEvent) string {
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	return fmt.Sprint(types)
}

// TestInboundOversized: a message over maxInboundMessageBytes is ingested
// from its header block alone (ProcessOversized) — visible notice article,
// real Message-ID dedup, threading anchor, and the auto-ack still fires so
// the customer gets their ticket number.
func TestInboundOversized(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)
	mb := makeMailbox(t, mailboxOpts{autoAck: true})

	from := uniq("bigsender") + "@example.test"
	msgID := uniq("huge") + "@example.test"
	full := inboundMail{
		from:      "Big Sender <" + from + ">",
		subject:   "Enormous attachment inbound",
		messageID: msgID,
		body:      "pretend this is 200 MiB\n",
	}.raw()
	sep := bytes.Index(full, []byte("\r\n\r\n"))
	if sep < 0 {
		t.Fatalf("no header/body separator in test message")
	}
	header := full[:sep+4] // what BODY.PEEK[HEADER] returns

	res, err := e.ProcessOversized(ctx, mb, header, 200<<20)
	if err != nil {
		t.Fatalf("ProcessOversized: %v", err)
	}
	if !res.NewTicket || res.Suppressed {
		t.Errorf("oversized ingest result = %+v, want new unsuppressed ticket", res)
	}
	a := getArticle(t, res.ArticleID)
	if !strings.Contains(a.BodyText, "Message too large to ingest") {
		t.Errorf("oversized article body = %q, want size notice", a.BodyText)
	}
	if got := textVal(a.MessageID); got != msgID {
		t.Errorf("oversized article message id = %q, want %q (real header id)", got, msgID)
	}

	// Auto-ack still enqueued: the customer must learn their ticket number.
	var acked bool
	for _, j := range listJobs(t, AutoAckArgs{}.Kind()) {
		if j.Args["ticket_id"] == res.TicketID.String() {
			acked = true
		}
	}
	if !acked {
		t.Errorf("no auto-ack enqueued for oversized new ticket")
	}

	// Redelivery of the same header block dedups exactly-once.
	again, err := e.ProcessOversized(ctx, mb, header, 200<<20)
	if err != nil {
		t.Fatalf("oversized redelivery: %v", err)
	}
	if !again.Duplicate {
		t.Errorf("oversized redelivery not deduped: %+v", again)
	}

	// The stored id still anchors reply threading.
	reply, err := e.ProcessMessage(ctx, mb, inboundMail{
		from:      "Big Sender <" + from + ">",
		subject:   "Re: Enormous attachment inbound",
		messageID: uniq("followup") + "@example.test",
		headers:   []string{"In-Reply-To: <" + msgID + ">"},
	}.raw())
	if err != nil {
		t.Fatalf("reply to oversized: %v", err)
	}
	if reply.NewTicket || reply.TicketID != res.TicketID {
		t.Errorf("reply did not thread onto the oversized ticket: %+v", reply)
	}
}

// TestInboundGiantMessageID: an attacker-length Message-ID (> the btree
// index entry limit) must not error the ingest — it is replaced by the
// content-hash synthesis, which keeps redelivery dedup exact and keeps the
// stored id safe for outbound References replay.
func TestInboundGiantMessageID(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	mb := makeMailbox(t, mailboxOpts{})

	giant := strings.Repeat("a", 3*1024) + "@example.test"
	raw := inboundMail{
		from:      "Gina <" + uniq("gina") + "@example.test>",
		subject:   "My mailer writes novels into Message-ID",
		messageID: giant,
	}.raw()

	res, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("ProcessMessage with giant id: %v", err)
	}
	a := getArticle(t, res.ArticleID)
	stored := textVal(a.MessageID)
	if !strings.HasSuffix(stored, "@synthesized.slatedesk.invalid") {
		t.Errorf("giant id stored verbatim (%d bytes): %q…", len(stored), stored[:60])
	}
	if len(stored) > maxMessageIDLen {
		t.Errorf("stored id length %d exceeds cap %d", len(stored), maxMessageIDLen)
	}

	again, err := e.ProcessMessage(ctx, mb, raw)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if !again.Duplicate {
		t.Errorf("giant-id redelivery not deduped: %+v", again)
	}
}
