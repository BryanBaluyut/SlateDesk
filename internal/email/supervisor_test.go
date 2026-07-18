package email

// Supervisor tests run against a real in-process IMAP server (go-imap's
// imapserver + memory backend): dial, SASL PLAIN auth, SELECT, UNSEEN
// sweep, IDLE wakeups, \Seen-after-commit — the whole connection loop,
// no GreenMail dependency.

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/google/uuid"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// fakeIMAP is an in-process IMAP server with one user.
type fakeIMAP struct {
	user *imapmemserver.User
	addr *net.TCPAddr
}

func newFakeIMAP(t *testing.T, username, password string) *fakeIMAP {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(username, password)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	mem.AddUser(user)

	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("imap listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return &fakeIMAP{user: user, addr: ln.Addr().(*net.TCPAddr)}
}

type memLiteral struct {
	r *bytes.Reader
}

func (l memLiteral) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l memLiteral) Size() int64                { return l.r.Size() }

// deliver appends a message to the user's INBOX (as if the MTA delivered
// it); connected IDLE sessions get a unilateral EXISTS.
func (f *fakeIMAP) deliver(t *testing.T, raw []byte) {
	t.Helper()
	if _, err := f.user.Append("INBOX", memLiteral{bytes.NewReader(raw)}, &imap.AppendOptions{}); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// fastConfig shrinks every supervisor interval for tests.
func fastConfig() SupervisorConfig {
	return SupervisorConfig{
		ReconcileInterval: 150 * time.Millisecond,
		LeaseTTL:          3 * time.Second,
		LeaseRenewEvery:   500 * time.Millisecond,
		PollInterval:      250 * time.Millisecond,
		IdleRestart:       time.Minute,
		ReconnectMin:      50 * time.Millisecond,
		ReconnectMax:      500 * time.Millisecond,
	}
}

// waitFor polls cond until true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// articleCount counts a ticket-less global article count per mailbox via
// email_message_ids (simplest cross-ticket probe).
func inboundCount(t *testing.T, mailboxID uuid.UUID) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		"SELECT count(*) FROM email_message_ids WHERE mailbox_id = $1 AND direction = 'inbound'",
		mailboxID).Scan(&n); err != nil {
		t.Fatalf("count inbound: %v", err)
	}
	return n
}

// TestSupervisorEndToEnd: the supervisor adopts an active mailbox via
// lease, connects to the IMAP server, ingests pre-existing UNSEEN mail,
// picks up new mail during IDLE, marks everything \Seen only after
// ingest, and updates mailbox health.
func TestSupervisorEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)

	addr := uniq("imapbox") + "@slatedesk.test"
	f := newFakeIMAP(t, addr, "secret")
	mb := makeMailbox(t, mailboxOpts{
		address:  addr,
		imapHost: "127.0.0.1",
		imapPort: int32(f.addr.Port),
	})

	// One message waiting before the supervisor exists.
	f.deliver(t, inboundMail{
		from:      "Pre <" + uniq("pre") + "@example.test>",
		subject:   "Waiting in inbox",
		messageID: uniq("pre") + "@example.test",
	}.raw())

	sup := NewSupervisor(e, fastConfig())
	go func() { _ = sup.Run(ctx) }()

	waitFor(t, 15*time.Second, "first ingest", func() bool { return inboundCount(t, mb.ID) == 1 })

	// Lease is held by this supervisor.
	var owner string
	if err := testPool.QueryRow(ctx,
		"SELECT owner FROM mailbox_leases WHERE mailbox_id = $1", mb.ID).Scan(&owner); err != nil {
		t.Fatalf("lease row: %v", err)
	}
	if owner != sup.Owner() {
		t.Errorf("lease owner = %q, want %q", owner, sup.Owner())
	}

	// New mail while the connection IDLEs.
	f.deliver(t, inboundMail{
		from:      "Live <" + uniq("live") + "@example.test>",
		subject:   "Fresh mail",
		messageID: uniq("live") + "@example.test",
	}.raw())
	waitFor(t, 15*time.Second, "idle ingest", func() bool { return inboundCount(t, mb.ID) == 2 })

	// Health pill updated.
	waitFor(t, 10*time.Second, "health ok", func() bool {
		row, err := store.New(testPool).GetMailbox(ctx, mb.ID)
		if err != nil {
			return false
		}
		return row.LastPollAt.Valid && !row.LastError.Valid
	})

	// \Seen-after-commit: after a few extra sweeps nothing re-ingests and
	// the mailbox holds no UNSEEN messages (dedup would hide a missing
	// \Seen, so count both).
	time.Sleep(3 * fastConfig().PollInterval)
	if n := inboundCount(t, mb.ID); n != 2 {
		t.Errorf("inbound rows = %d, want 2 (no re-ingest)", n)
	}
	status, err := f.user.Status("INBOX", &imap.StatusOptions{NumUnseen: true})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.NumUnseen == nil || *status.NumUnseen != 0 {
		t.Errorf("unseen = %v, want 0 (\\Seen set after commit)", status.NumUnseen)
	}

	// Shutdown releases the lease.
	cancel()
	waitFor(t, 10*time.Second, "lease release", func() bool {
		var n int
		if err := testPool.QueryRow(context.Background(),
			"SELECT count(*) FROM mailbox_leases WHERE mailbox_id = $1", mb.ID).Scan(&n); err != nil {
			return false
		}
		return n == 0
	})
}

// TestSupervisorLeaseExclusion: a live foreign lease keeps the supervisor
// out; when it lapses, the mailbox is adopted at a later reconcile.
func TestSupervisorLeaseExclusion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)

	addr := uniq("leased") + "@slatedesk.test"
	f := newFakeIMAP(t, addr, "secret")
	mb := makeMailbox(t, mailboxOpts{
		address:  addr,
		imapHost: "127.0.0.1",
		imapPort: int32(f.addr.Port),
	})

	// Another worker holds the lease.
	if _, err := store.New(testPool).ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mb.ID, Owner: "other-host:999", TtlSeconds: 2,
	}); err != nil {
		t.Fatalf("foreign claim: %v", err)
	}

	f.deliver(t, inboundMail{
		from:      "Blocked <" + uniq("blocked") + "@example.test>",
		subject:   "Should wait",
		messageID: uniq("blocked") + "@example.test",
	}.raw())

	sup := NewSupervisor(e, fastConfig())
	go func() { _ = sup.Run(ctx) }()

	// While the foreign lease lives, nothing ingests.
	time.Sleep(1 * time.Second)
	if n := inboundCount(t, mb.ID); n != 0 {
		t.Fatalf("supervisor processed a mailbox it does not own (%d rows)", n)
	}

	// After the 2s foreign TTL expires, a reconcile adopts and ingests.
	waitFor(t, 15*time.Second, "adoption after lease expiry", func() bool {
		return inboundCount(t, mb.ID) == 1
	})
}

// TestSupervisorKick: a mailbox created after startup is picked up
// immediately on Kick (no waiting for ReconcileInterval).
func TestSupervisorKick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := newTestEngine(t)
	wireInsertOnlyRiver(t, e)

	// Slow reconcile so only Kick can explain a fast pickup.
	cfg := fastConfig()
	cfg.ReconcileInterval = time.Hour
	sup := NewSupervisor(e, cfg)
	go func() { _ = sup.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // let the first (empty) reconcile pass

	addr := uniq("kicked") + "@slatedesk.test"
	f := newFakeIMAP(t, addr, "secret")
	mb := makeMailbox(t, mailboxOpts{
		address:  addr,
		imapHost: "127.0.0.1",
		imapPort: int32(f.addr.Port),
	})
	f.deliver(t, inboundMail{
		from:      "Kick <" + uniq("kick") + "@example.test>",
		subject:   "Via kick",
		messageID: uniq("kick") + "@example.test",
	}.raw())

	sup.Kick()
	waitFor(t, 15*time.Second, "kick ingest", func() bool { return inboundCount(t, mb.ID) == 1 })
}
