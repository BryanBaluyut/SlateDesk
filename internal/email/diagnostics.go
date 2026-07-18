// Live mailbox connectivity tests behind the admin screen's Fetch test /
// Send test buttons (architecture doc §5.3). Both perform REAL dials with
// the stored credentials; the HTTP layer bounds them with a context
// deadline and renders {ok, detail, latency_ms}. Errors name the failing
// step and never embed credential material themselves — the handler
// additionally scrubs decrypted secrets before echoing any detail.
//
// Trust boundary (deliberate, documented): imap_host/smtp_host are
// admin-supplied and dialed VERBATIM — no allow-list and no RFC 1918 /
// link-local guard — so an admin can point a test (or the supervisor) at
// internal targets and read back latency plus scrubbed, truncated error
// text: SSRF-style internal probing. That is within this product's trust
// model: mailbox mutation is admin-only, and admins already hold the
// instance's mail credentials and every mailbox's full config; the
// supervisor will dial whatever they configure regardless of these test
// endpoints. Revisit only if a less-than-admin role ever gains mailbox
// mutation rights.
package email

import (
	"context"
	"fmt"
	"net"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/google/uuid"
	"github.com/wneessen/go-mail"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// TestFetch performs a live IMAP check for the mailbox: dial per
// imap_tls_mode, authenticate with the stored credentials (minting/
// refreshing OAuth tokens as needed), and SELECT INBOX. Nil error = the
// mailbox is fetchable. Respect ctx: give it a deadline and the dial and
// the blocking protocol reads are torn down when it passes.
func (e *Engine) TestFetch(ctx context.Context, mb store.Mailbox) error {
	dialer := &net.Dialer{}
	if deadline, ok := ctx.Deadline(); ok {
		dialer.Deadline = deadline
	}
	opts := &imapclient.Options{Dialer: dialer}

	addr := fmt.Sprintf("%s:%d", mb.ImapHost, mb.ImapPort)
	var c *imapclient.Client
	var err error
	switch mb.ImapTlsMode {
	case store.MailTlsModeTls:
		c, err = imapclient.DialTLS(addr, opts)
	case store.MailTlsModeStarttls:
		c, err = imapclient.DialStartTLS(addr, opts)
	case store.MailTlsModeNone:
		c, err = imapclient.DialInsecure(addr, opts)
	default:
		return fmt.Errorf("unknown IMAP TLS mode %q", mb.ImapTlsMode)
	}
	if err != nil {
		return fmt.Errorf("IMAP dial %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	// The dialer deadline only covers the TCP/TLS setup; this watchdog
	// tears down the blocking protocol reads when ctx dies (same pattern
	// as the supervisor's connection loop).
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-watchdogDone:
		}
	}()

	if err := e.auth.AuthenticateIMAP(ctx, c, mb); err != nil {
		return fmt.Errorf("IMAP authenticate: %w", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("IMAP select INBOX: %w", err)
	}
	// The test has passed; a noisy logout must not fail it.
	_ = c.Logout().Wait()
	return nil
}

// TestSend sends a short test message to toAddr through the mailbox's
// SMTP config (live dial + authentication). The mail carries the
// loop-guard headers (Auto-Submitted, X-Auto-Response-Suppress) so it can
// never start an autoresponder loop, and it is NOT recorded on any ticket
// or in email_message_ids — it is a connectivity probe, not conversation.
func (e *Engine) TestSend(ctx context.Context, mb store.Mailbox, toAddr string) error {
	m := mail.NewMsg()
	fromName := mb.FromDisplayName
	if fromName == "" {
		fromName = mb.Name
	}
	if err := m.FromFormat(fromName, mb.EmailAddress); err != nil {
		return fmt.Errorf("bad from address %q: %w", mb.EmailAddress, err)
	}
	if err := m.To(toAddr); err != nil {
		return fmt.Errorf("bad recipient address %q: %w", toAddr, err)
	}
	m.Subject("SlateDesk test message")
	m.SetMessageIDWithValue(uuid.NewString() + "@" + mailboxDomain(mb))
	m.SetGenHeader("Auto-Submitted", "auto-generated")
	m.SetGenHeader("X-Auto-Response-Suppress", "All")
	m.SetBodyString(mail.TypeTextPlain, fmt.Sprintf(
		"This is a test message from SlateDesk confirming that the mailbox %s "+
			"can send mail.\n\nNo action is required; replies are not monitored.\n",
		mb.EmailAddress))

	if err := e.smtpSend(ctx, mb, m); err != nil {
		return fmt.Errorf("SMTP send: %w", err)
	}
	return nil
}
