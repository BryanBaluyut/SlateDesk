// Per-mailbox goroutine lifecycle (architecture doc §4): each active
// mailbox is a supervised long-lived goroutine holding the IMAP IDLE
// connection with a poll fallback. The DB lease (mailbox_leases) provides
// OWNERSHIP only — exactly one worker process runs a given mailbox at any
// replica count; on worker death the lease lapses and another worker
// adopts the mailbox at its next reconcile.
package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// SupervisorConfig carries the supervisor's timing knobs. Zero values take
// the production defaults; tests shrink them.
type SupervisorConfig struct {
	// ReconcileInterval: how often the active-mailbox set is re-read from
	// the DB (Kick() forces an immediate pass).
	ReconcileInterval time.Duration // default 15s
	// LeaseTTL / LeaseRenewEvery: ownership lease lifetime and renewal
	// cadence. TTL spans three renewal periods, so two misses survive but
	// a dead process loses the mailbox within ~40s. Worst-case failover
	// is LeaseTTL + ReconcileInterval (holder dies right after renewing;
	// a peer claims on its first reconcile tick past expiry) — the
	// defaults keep that under a minute.
	LeaseTTL        time.Duration // default 40s
	LeaseRenewEvery time.Duration // default 12s
	// PollInterval: fallback UNSEEN sweep while IDLE is quiet.
	PollInterval time.Duration // default 60s
	// IdleRestart: maximum lifetime of one IDLE command (RFC 2177 asks
	// clients to re-issue at least every 29 minutes).
	IdleRestart time.Duration // default 25m
	// ReconnectMin/Max: capped exponential backoff for dial/auth failures.
	ReconnectMin time.Duration // default 250ms
	ReconnectMax time.Duration // default 60s
}

func (c SupervisorConfig) withDefaults() SupervisorConfig {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.ReconcileInterval, 15*time.Second)
	def(&c.LeaseTTL, 40*time.Second)
	def(&c.LeaseRenewEvery, 12*time.Second)
	def(&c.PollInterval, 60*time.Second)
	def(&c.IdleRestart, 25*time.Minute)
	def(&c.ReconnectMin, 250*time.Millisecond)
	def(&c.ReconnectMax, 60*time.Second)
	return c
}

// Supervisor owns the set of mailbox runner goroutines in this process.
type Supervisor struct {
	engine *Engine
	cfg    SupervisorConfig
	owner  string
	kick   chan struct{}

	mu      sync.Mutex
	runners map[uuid.UUID]*mailboxRunner
}

// NewSupervisor builds a Supervisor for engine. The lease owner identity
// is hostname+pid: unique per process, stable for its lifetime, and
// human-readable in the mailbox_leases table.
func NewSupervisor(engine *Engine, cfg SupervisorConfig) *Supervisor {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	return &Supervisor{
		engine:  engine,
		cfg:     cfg.withDefaults(),
		owner:   fmt.Sprintf("%s:%d", hostname, os.Getpid()),
		kick:    make(chan struct{}, 1),
		runners: make(map[uuid.UUID]*mailboxRunner),
	}
}

// Owner returns the lease owner identity (visible in mailbox_leases).
func (s *Supervisor) Owner() string { return s.owner }

// Kick forces an immediate reconcile (mailbox created/updated via the
// admin API — the handlers phase calls this instead of waiting out
// ReconcileInterval). Never blocks.
func (s *Supervisor) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx is canceled, then stops every runner and
// releases its leases. Blocking; run it on its own goroutine.
func (s *Supervisor) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.ReconcileInterval)
	defer ticker.Stop()

	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			s.stopAll()
			return ctx.Err()
		case <-ticker.C:
		case <-s.kick:
		}
	}
}

// reconcile diffs the DB's active mailboxes against the running set:
// starts runners for new mailboxes, restarts runners whose config changed
// (updated_at moved), stops runners for deactivated/deleted mailboxes.
// Runners that exited on their own (lease lost, fatal auth) are reaped and
// retried here — reconcile doubles as the retry loop for lease adoption.
func (s *Supervisor) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	boxes, err := s.engine.q.ListActiveMailboxes(ctx)
	if err != nil {
		slog.Error("email: list active mailboxes", "error", err)
		return
	}

	desired := make(map[uuid.UUID]store.Mailbox, len(boxes))
	for _, mb := range boxes {
		desired[mb.ID] = mb
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, r := range s.runners {
		mb, keep := desired[id]
		switch {
		case !keep:
			slog.Info("email: mailbox deactivated; stopping runner", "mailbox", r.mb.EmailAddress)
			r.stop()
			delete(s.runners, id)
		case r.done():
			delete(s.runners, id) // exited (lease lost etc.); restart below
		case !mb.UpdatedAt.Equal(r.mb.UpdatedAt):
			slog.Info("email: mailbox config changed; restarting runner", "mailbox", mb.EmailAddress)
			r.stop()
			delete(s.runners, id)
		}
	}

	for id, mb := range desired {
		if _, running := s.runners[id]; running {
			continue
		}
		r := newMailboxRunner(ctx, s.engine, s.cfg, s.owner, mb)
		s.runners[id] = r
		go r.run()
	}
}

func (s *Supervisor) stopAll() {
	s.mu.Lock()
	runners := make([]*mailboxRunner, 0, len(s.runners))
	for id, r := range s.runners {
		runners = append(runners, r)
		delete(s.runners, id)
	}
	s.mu.Unlock()
	for _, r := range runners {
		r.stop()
	}
}

// runningCount is a test hook.
func (s *Supervisor) runningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.runners {
		if !r.done() {
			n++
		}
	}
	return n
}

// mailboxRunner is one mailbox's goroutine: lease, connection loop, IDLE.
type mailboxRunner struct {
	engine *Engine
	cfg    SupervisorConfig
	owner  string
	mb     store.Mailbox

	// ctx/cancel are created at CONSTRUCTION, before the run goroutine is
	// scheduled: a stop() racing a just-started runner (deactivate/config
	// change kicked between reconcile's `go r.run()` and the goroutine
	// actually executing) must always have a context to cancel. Were the
	// context created inside run(), that stop() would cancel nothing and
	// then block forever on doneCh — while holding Supervisor.mu, wedging
	// every future reconcile.
	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan struct{}
}

func newMailboxRunner(parent context.Context, engine *Engine, cfg SupervisorConfig, owner string, mb store.Mailbox) *mailboxRunner {
	ctx, cancel := context.WithCancel(parent)
	return &mailboxRunner{engine: engine, cfg: cfg, owner: owner, mb: mb, ctx: ctx, cancel: cancel, doneCh: make(chan struct{})}
}

func (r *mailboxRunner) done() bool {
	select {
	case <-r.doneCh:
		return true
	default:
		return false
	}
}

// stop cancels the runner and waits for it to exit (lease released).
func (r *mailboxRunner) stop() {
	r.cancel()
	<-r.doneCh
}

// run claims the lease and, holding it, runs the reconnect loop. Exits
// when the lease is lost, a fatal condition occurs, or the runner's ctx is
// canceled. If the lease cannot be claimed (another worker owns the
// mailbox) it exits immediately; the supervisor retries at the next
// reconcile.
func (r *mailboxRunner) run() {
	defer close(r.doneCh)

	ctx, cancel := r.ctx, r.cancel
	defer cancel()

	if _, err := r.engine.q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID:  r.mb.ID,
		Owner:      r.owner,
		TtlSeconds: r.cfg.LeaseTTL.Seconds(),
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
			slog.Error("email: lease claim", "mailbox", r.mb.EmailAddress, "error", err)
		}
		return // someone else holds a live lease
	}
	slog.Info("email: mailbox adopted", "mailbox", r.mb.EmailAddress, "owner", r.owner)

	// Renewal loop: every LeaseRenewEvery. ANY renewal failure — expired
	// and taken over, DB down, context gone — cancels ctx, which tears
	// down the IMAP connection and stops all processing immediately: we
	// must never keep polling a mailbox we might no longer own.
	go func() {
		ticker := time.NewTicker(r.cfg.LeaseRenewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if _, err := r.engine.q.RenewMailboxLease(ctx, store.RenewMailboxLeaseParams{
				MailboxID:  r.mb.ID,
				Owner:      r.owner,
				TtlSeconds: r.cfg.LeaseTTL.Seconds(),
			}); err != nil {
				if ctx.Err() == nil {
					slog.Warn("email: lease renewal failed; stopping mailbox",
						"mailbox", r.mb.EmailAddress, "error", err)
				}
				cancel()
				return
			}
		}
	}()

	defer func() {
		// Graceful handover: drop the lease so a peer adopts immediately.
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
		defer releaseCancel()
		if _, err := r.engine.q.ReleaseMailboxLease(releaseCtx, store.ReleaseMailboxLeaseParams{
			MailboxID: r.mb.ID,
			Owner:     r.owner,
		}); err != nil {
			slog.Warn("email: lease release", "mailbox", r.mb.EmailAddress, "error", err)
		}
	}()

	// Connection loop with capped exponential backoff.
	backoff := r.cfg.ReconnectMin
	for {
		healthy, err := r.connectAndProcess(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("email: mailbox connection error", "mailbox", r.mb.EmailAddress, "error", err)
			r.setHealthError(ctx, err)
		}
		if healthy {
			backoff = r.cfg.ReconnectMin // made progress before failing
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > r.cfg.ReconnectMax {
			backoff = r.cfg.ReconnectMax
		}
	}
}

// connectAndProcess runs one IMAP connection: dial per tls_mode, SASL
// authenticate, SELECT INBOX, then sweep-then-IDLE until the connection or
// ctx dies. Returns healthy=true if at least one sweep succeeded (resets
// the reconnect backoff).
func (r *mailboxRunner) connectAndProcess(ctx context.Context) (healthy bool, err error) {
	// newMail is signaled by unilateral EXISTS updates during IDLE.
	newMail := make(chan struct{}, 1)
	opts := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(*imapclient.UnilateralDataMailbox) {
				select {
				case newMail <- struct{}{}:
				default:
				}
			},
		},
	}

	addr := fmt.Sprintf("%s:%d", r.mb.ImapHost, r.mb.ImapPort)
	var c *imapclient.Client
	switch r.mb.ImapTlsMode {
	case store.MailTlsModeTls:
		c, err = imapclient.DialTLS(addr, opts)
	case store.MailTlsModeStarttls:
		c, err = imapclient.DialStartTLS(addr, opts)
	case store.MailTlsModeNone:
		c, err = imapclient.DialInsecure(addr, opts)
	default:
		return false, fmt.Errorf("unknown imap tls mode %q", r.mb.ImapTlsMode)
	}
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()

	// Tear the blocking protocol reads down when ctx dies.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-watchdogDone:
		}
	}()

	if err := r.engine.auth.AuthenticateIMAP(ctx, c, r.mb); err != nil {
		return false, fmt.Errorf("authenticate: %w", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		return false, fmt.Errorf("select INBOX: %w", err)
	}

	// Servers without the IDLE extension (RFC 2177) degrade to
	// PollInterval-paced sweeps on this same connection. Issuing IDLE
	// anyway would fail the connection right after a successful sweep set
	// healthy=true and reset the backoff — a full dial/TLS/auth/SELECT
	// reconnect storm at ReconnectMin cadence instead of calm polling.
	supportsIdle := c.Caps().Has(imap.CapIdle)

	idleDeadline := time.NewTimer(r.cfg.IdleRestart)
	defer idleDeadline.Stop()

	for {
		if err := r.sweep(ctx, c); err != nil {
			return healthy, err
		}
		healthy = true

		if !supportsIdle {
			select {
			case <-ctx.Done():
				return healthy, ctx.Err()
			case <-time.After(r.cfg.PollInterval):
			}
			continue
		}

		// IDLE between sweeps; wake on new mail, the poll fallback, or
		// the IDLE-restart deadline (each wake re-issues IDLE, so no IDLE
		// session ever outlives IdleRestart).
		idleCmd, err := c.Idle()
		if err != nil {
			return healthy, fmt.Errorf("idle: %w", err)
		}
		if !idleDeadline.Stop() {
			select {
			case <-idleDeadline.C:
			default:
			}
		}
		idleDeadline.Reset(r.cfg.IdleRestart)
		select {
		case <-ctx.Done():
			_ = idleCmd.Close()
			_ = idleCmd.Wait()
			return healthy, ctx.Err()
		case <-newMail:
		case <-time.After(r.cfg.PollInterval):
		case <-idleDeadline.C:
		}
		if err := idleCmd.Close(); err != nil {
			return healthy, fmt.Errorf("end idle: %w", err)
		}
		if err := idleCmd.Wait(); err != nil {
			return healthy, fmt.Errorf("idle: %w", err)
		}
	}
}

// sweep ingests every UNSEEN message: fetch full body, ProcessMessage
// (ONE transaction), and — only after that commit — set \Seen. A message
// that fails processing stays UNSEEN and is retried next sweep; the
// others still proceed.
func (r *mailboxRunner) sweep(ctx context.Context, c *imapclient.Client) error {
	sd, err := c.UIDSearch(&imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
	}, nil).Wait()
	if err != nil {
		return fmt.Errorf("search unseen: %w", err)
	}

	var sweepErr error
	for _, uid := range sd.AllUIDs() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.ingestOne(ctx, c, uid); err != nil {
			slog.Error("email: message ingest failed; leaving unseen",
				"mailbox", r.mb.EmailAddress, "uid", uint32(uid), "error", err)
			sweepErr = err
		}
	}

	if sweepErr != nil {
		r.setHealthError(ctx, sweepErr)
		return nil // connection is fine; don't tear it down
	}
	if err := r.engine.q.SetMailboxHealthOK(ctx, r.mb.ID); err != nil {
		slog.Warn("email: set mailbox health", "mailbox", r.mb.EmailAddress, "error", err)
	}
	return nil
}

// ingestOne fetches one message and runs the inbound pipeline. \Seen is
// stored only after ProcessMessage committed (or reported a duplicate) —
// the at-least-once / exactly-once contract. Every body fetch here MUST
// set Peek: a plain BODY[] fetch makes the server flag the message \Seen
// implicitly at FETCH time — before the ingest transaction commits — so a
// failed ingest would vanish from every future UNSEEN sweep and the mail
// would be silently lost.
func (r *mailboxRunner) ingestOne(ctx context.Context, c *imapclient.Client, uid imap.UID) error {
	uidSet := imap.UIDSetNum(uid)

	// Size gate BEFORE any body bytes leave the server: enmime decodes
	// every MIME part into memory, so fetching an unbounded message is an
	// O(message size) allocation per concurrent runner. Oversized mail is
	// ingested from its header block alone (see ProcessOversized).
	sized, err := c.Fetch(uidSet, &imap.FetchOptions{
		UID:        true,
		RFC822Size: true,
	}).Collect()
	if err != nil {
		return fmt.Errorf("fetch size uid %d: %w", uint32(uid), err)
	}
	if len(sized) == 0 {
		return fmt.Errorf("fetch size uid %d: empty response", uint32(uid))
	}

	var res InboundResult
	if size := sized[0].RFC822Size; size > maxInboundMessageBytes {
		slog.Warn("email: message over size cap; ingesting header-only notice",
			"mailbox", r.mb.EmailAddress, "uid", uint32(uid), "size", size, "cap", maxInboundMessageBytes)
		msgs, err := c.Fetch(uidSet, &imap.FetchOptions{
			UID: true,
			BodySection: []*imap.FetchItemBodySection{
				{Specifier: imap.PartSpecifierHeader, Peek: true},
			},
		}).Collect()
		if err != nil {
			return fmt.Errorf("fetch header uid %d: %w", uint32(uid), err)
		}
		if len(msgs) == 0 || len(msgs[0].BodySection) == 0 {
			return fmt.Errorf("fetch header uid %d: empty response", uint32(uid))
		}
		res, err = r.engine.ProcessOversized(ctx, r.mb, msgs[0].BodySection[0].Bytes, size)
		if err != nil {
			return err
		}
	} else {
		msgs, err := c.Fetch(uidSet, &imap.FetchOptions{
			UID:         true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil {
			return fmt.Errorf("fetch uid %d: %w", uint32(uid), err)
		}
		if len(msgs) == 0 || len(msgs[0].BodySection) == 0 {
			return fmt.Errorf("fetch uid %d: empty response", uint32(uid))
		}
		res, err = r.engine.ProcessMessage(ctx, r.mb, msgs[0].BodySection[0].Bytes)
		if err != nil {
			return err
		}
	}
	if res.Duplicate {
		slog.Info("email: duplicate delivery skipped", "mailbox", r.mb.EmailAddress, "uid", uint32(uid))
	}

	// AFTER commit: mark seen (silent — no untagged FETCH echo needed).
	// If this store fails (crash, connection drop) the message is
	// redelivered UNSEEN and the dedup arm skips it.
	if err := c.Store(uidSet, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagSeen},
	}, nil).Close(); err != nil {
		return fmt.Errorf("mark seen uid %d: %w", uint32(uid), err)
	}
	return nil
}

// setHealthError records a failure on the mailbox health pill.
func (r *mailboxRunner) setHealthError(ctx context.Context, cause error) {
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if err := r.engine.q.SetMailboxHealthError(context.WithoutCancel(ctx), store.SetMailboxHealthErrorParams{
		ID:        r.mb.ID,
		LastError: pgtype.Text{String: msg, Valid: true},
	}); err != nil {
		slog.Warn("email: set mailbox health error", "mailbox", r.mb.EmailAddress, "error", err)
	}
}
