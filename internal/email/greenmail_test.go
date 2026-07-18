package email

// Opt-in live smoke test against a local GreenMail (real SMTP+IMAP server
// in Docker). The regular suite must NOT depend on GreenMail, so this
// whole file skips unless SLATEDESK_GREENMAIL_TEST=1.
//
//	SLATEDESK_GREENMAIL_TEST=1 go test ./internal/email -run TestGreenMail -v
//
// Expects GreenMail with SMTP :3025, IMAP :3143 (no TLS) and REST API
// :8085, users support@slatedesk.test/secret + customer@example.test/secret
// (greenmail.setup.test.all auto-creates any login).

import (
	"context"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

var gmEnv = os.Getenv("SLATEDESK_GREENMAIL_TEST")

const (
	gmSMTP    = "127.0.0.1:3025"
	gmIMAP    = "127.0.0.1:3143"
	gmREST    = "http://127.0.0.1:8085/api"
	gmSupport = "support@slatedesk.test"
	gmCust    = "customer@example.test"
	gmPass    = "secret"
	// GreenMail's -Dgreenmail.users=support:secret@slatedesk.test creates
	// LOGIN id "support" (short name), not the full address.
	gmSupportLogin = "support"
	gmCustLogin    = "customer"
)

// gmPurge wipes all GreenMail messages.
func gmPurge(t *testing.T) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, gmREST+"/mail/purge", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("greenmail purge: %v (is GreenMail running?)", err)
	}
	_ = resp.Body.Close()
}

// gmSend delivers raw mail into GreenMail over real SMTP.
func gmSend(t *testing.T, from, to string, raw []byte) {
	t.Helper()
	if err := smtp.SendMail(gmSMTP, nil, from, []string{to}, raw); err != nil {
		t.Fatalf("smtp send: %v", err)
	}
}

// gmInbox fetches every message in a user's INBOX (envelope + body).
func gmInbox(t *testing.T, user, pass string) []sinkMessage {
	t.Helper()
	c, err := imapclient.DialInsecure(gmIMAP, nil)
	if err != nil {
		t.Fatalf("imap dial: %v", err)
	}
	defer c.Close()
	if err := c.Login(user, pass).Wait(); err != nil {
		t.Fatalf("imap login %s: %v", user, err)
	}
	sel, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatalf("imap select: %v", err)
	}
	if sel.NumMessages == 0 {
		return nil
	}
	var seq imap.SeqSet
	seq.AddRange(1, sel.NumMessages)
	msgs, err := c.Fetch(seq, &imap.FetchOptions{
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		t.Fatalf("imap fetch: %v", err)
	}
	var out []sinkMessage
	for _, m := range msgs {
		if len(m.BodySection) > 0 {
			out = append(out, sinkMessage{Data: string(m.BodySection[0].Bytes)})
		}
	}
	return out
}

// gmWaitInbox polls a mailbox until it holds n messages.
func gmWaitInbox(t *testing.T, user, pass string, n int, timeout time.Duration) []sinkMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msgs := gmInbox(t, user, pass)
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("greenmail: %s has %d messages, want %d", user, len(msgs), n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestGreenMailLiveSmoke: the full loop against a real mail server —
// customer mail -> supervisor IMAP ingest -> ticket + auto-ack out via
// real SMTP -> agent reply -> customer replies to OUR Message-ID ->
// threads into the same ticket.
func TestGreenMailLiveSmoke(t *testing.T) {
	if gmEnv == "" {
		t.Skip("set SLATEDESK_GREENMAIL_TEST=1 to run the live GreenMail smoke test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gmPurge(t)
	e := newTestEngine(t)
	clearJobs(t)
	startWorkerRiver(t, e)

	// Mailbox = the real GreenMail endpoints, basic auth, no TLS.
	enc, err := EncryptCredentials(testBox, Credentials{Password: gmPass})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mb, err := store.New(testPool).CreateMailbox(ctx, store.CreateMailboxParams{
		Name: "GreenMail", EmailAddress: gmSupport, Active: true,
		AuthKind: store.MailboxAuthKindBasic,
		ImapHost: "127.0.0.1", ImapPort: 3143, ImapTlsMode: store.MailTlsModeNone, ImapUsername: gmSupportLogin,
		SmtpHost: "127.0.0.1", SmtpPort: 3025, SmtpTlsMode: store.MailTlsModeNone, SmtpUsername: gmSupportLogin,
		CredentialsEnc: enc, FromDisplayName: "SlateDesk Support",
		Signature: "SlateDesk Support Team", AutoAckEnabled: true,
	})
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}

	sup := NewSupervisor(e, fastConfig())
	go func() { _ = sup.Run(ctx) }()

	// 1. Customer emails support.
	custMsgID := uniq("gm-cust") + "@example.test"
	gmSend(t, gmCust, gmSupport, rawMessage([]string{
		"From: GreenMail Customer <" + gmCust + ">",
		"To: " + gmSupport,
		"Subject: Live smoke: printer literally on fire",
		"Message-ID: <" + custMsgID + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}, "This is a live GreenMail message.\n"))

	waitFor(t, 30*time.Second, "live ingest", func() bool { return inboundCount(t, mb.ID) >= 1 })
	row, err := store.New(testPool).GetEmailMessageID(ctx, custMsgID)
	if err != nil {
		t.Fatalf("ingested id: %v", err)
	}
	ticketID := row.TicketID
	number := getTicket(t, ticketID).Number

	// 2. Auto-ack lands in the customer's real inbox, token + loop-guard
	// headers intact.
	ackMsgs := gmWaitInbox(t, gmCustLogin, gmPass, 1, 30*time.Second)
	ack := ackMsgs[0]
	if !strings.Contains(ack.header("Subject"), "[SD-"+number+"]") {
		t.Errorf("ack subject = %q", ack.header("Subject"))
	}
	if ack.header("Auto-Submitted") != "auto-replied" {
		t.Errorf("ack Auto-Submitted = %q", ack.header("Auto-Submitted"))
	}

	// 3. Agent reply through the ticket service goes out via real SMTP.
	agent := makeUser(t, store.UserRoleAgent)
	svc := ticket.NewService(testPool)
	svc.SetMailer(e)
	if _, err := svc.AddArticle(ctx, ticketID, ticket.ArticleInput{
		AuthorID: &agent.ID, SenderType: store.ArticleSenderAgent,
		Channel: store.ArticleChannelWeb, BodyText: "We are on it — live smoke reply.",
	}); err != nil {
		t.Fatalf("AddArticle: %v", err)
	}
	replies := gmWaitInbox(t, gmCustLogin, gmPass, 2, 30*time.Second)
	var reply sinkMessage
	for _, m := range replies {
		if strings.Contains(m.Data, "live smoke reply") {
			reply = m
		}
	}
	ourID := canonicalMessageID(reply.header("Message-ID"))
	if ourID == "" {
		t.Fatalf("agent reply not delivered to customer inbox")
	}
	if got := canonicalMessageID(reply.header("In-Reply-To")); got != custMsgID {
		t.Errorf("reply In-Reply-To = %q, want %q", got, custMsgID)
	}

	// 4. Customer answers OUR Message-ID (v1's broken case) — must thread
	// into the same ticket, not open a new one.
	gmSend(t, gmCust, gmSupport, rawMessage([]string{
		"From: GreenMail Customer <" + gmCust + ">",
		"To: " + gmSupport,
		"Subject: Re: whatever my mail client did to the subject",
		"Message-ID: <" + uniq("gm-reply") + "@example.test>",
		"In-Reply-To: <" + ourID + ">",
		"References: <" + custMsgID + "> <" + ourID + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
	}, "Replying to your reply.\n"))

	waitFor(t, 30*time.Second, "threaded live reply", func() bool { return inboundCount(t, mb.ID) >= 2 })
	var n int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM articles WHERE ticket_id = $1", ticketID).Scan(&n); err != nil {
		t.Fatalf("count articles: %v", err)
	}
	// customer + ack + agent reply + threaded customer reply = 4.
	if n != 4 {
		t.Errorf("articles on live ticket = %d, want 4 (threading failed?)", n)
	}
	if got := getTicket(t, ticketID).Status; got != store.TicketStatusOpen {
		t.Errorf("live ticket status = %q, want open (customer reply flips waiting->open)", got)
	}
}
