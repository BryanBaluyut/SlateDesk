package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BryanBaluyut/slatedesk/internal/store"
)

// createTestMailbox inserts a minimal basic-auth mailbox and returns its id.
func createTestMailbox(t *testing.T, q *store.Queries) uuid.UUID {
	t.Helper()
	mb, err := q.CreateMailbox(context.Background(), store.CreateMailboxParams{
		Name:           "Support",
		EmailAddress:   fmt.Sprintf("support-%s@slatedesk.test", uuid.NewString()[:8]),
		Active:         true,
		AuthKind:       store.MailboxAuthKindBasic,
		ImapHost:       "127.0.0.1",
		ImapPort:       3143,
		ImapTlsMode:    store.MailTlsModeNone,
		ImapUsername:   "support@slatedesk.test",
		SmtpHost:       "127.0.0.1",
		SmtpPort:       3025,
		SmtpTlsMode:    store.MailTlsModeNone,
		SmtpUsername:   "support@slatedesk.test",
		CredentialsEnc: "v1:not-a-real-ciphertext",
	})
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	return mb.ID
}

func TestClaimMailboxLease(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	mailboxID := createTestMailbox(t, q)

	// First claim wins.
	leaseA, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 60,
	})
	if err != nil {
		t.Fatalf("worker-a initial claim: %v", err)
	}
	if leaseA.Owner != "worker-a" {
		t.Fatalf("lease owner = %q, want worker-a", leaseA.Owner)
	}

	// A rival cannot take a live lease.
	_, err = q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-b", TtlSeconds: 60,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("worker-b claim on live lease: err = %v, want pgx.ErrNoRows", err)
	}

	// The holder can re-claim its own live lease (owner = me path).
	leaseA2, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 120,
	})
	if err != nil {
		t.Fatalf("worker-a re-claim: %v", err)
	}
	if !leaseA2.ExpiresAt.After(leaseA.ExpiresAt) {
		t.Fatalf("re-claim did not extend: %v -> %v", leaseA.ExpiresAt, leaseA2.ExpiresAt)
	}
}

func TestClaimMailboxLeaseTakeoverOnlyAfterExpiry(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	mailboxID := createTestMailbox(t, q)

	// worker-a claims with a tiny TTL.
	if _, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 0.2,
	}); err != nil {
		t.Fatalf("worker-a claim: %v", err)
	}

	// Before expiry: takeover refused.
	if _, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-b", TtlSeconds: 60,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("takeover before expiry: err = %v, want pgx.ErrNoRows", err)
	}

	// After expiry: takeover succeeds. (Expiry compares against the DB
	// clock, same clock that set expires_at, so a plain sleep is sound.)
	time.Sleep(300 * time.Millisecond)
	lease, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-b", TtlSeconds: 60,
	})
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if lease.Owner != "worker-b" {
		t.Fatalf("lease owner after takeover = %q, want worker-b", lease.Owner)
	}

	// The old holder's renewal must now fail: it lost the mailbox.
	if _, err := q.RenewMailboxLease(ctx, store.RenewMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 60,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale owner renew: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestClaimMailboxLeaseContention(t *testing.T) {
	// Two fake owners fight over one mailbox at the same instant; exactly
	// one may win. Repeated a few times because the interesting schedule
	// (both hitting INSERT ... ON CONFLICT concurrently) is timing-dependent.
	ctx := context.Background()
	q := store.New(testPool)

	for round := range 5 {
		mailboxID := createTestMailbox(t, q)

		var (
			wg   sync.WaitGroup
			wins sync.Map
		)
		start := make(chan struct{})
		for _, owner := range []string{"worker-a", "worker-b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
					MailboxID: mailboxID, Owner: owner, TtlSeconds: 60,
				})
				switch {
				case err == nil:
					wins.Store(owner, true)
				case errors.Is(err, pgx.ErrNoRows):
					// lost the race; fine
				default:
					t.Errorf("round %d: %s claim: unexpected error: %v", round, owner, err)
				}
			}()
		}
		close(start)
		wg.Wait()

		var winners []string
		wins.Range(func(k, _ any) bool {
			winners = append(winners, k.(string))
			return true
		})
		if len(winners) != 1 {
			t.Fatalf("round %d: want exactly one winner, got %v", round, winners)
		}

		// The stored lease belongs to the winner.
		var owner string
		if err := testPool.QueryRow(ctx,
			"SELECT owner FROM mailbox_leases WHERE mailbox_id = $1", mailboxID,
		).Scan(&owner); err != nil {
			t.Fatalf("round %d: read lease: %v", round, err)
		}
		if owner != winners[0] {
			t.Fatalf("round %d: stored owner %q != winner %q", round, owner, winners[0])
		}
	}
}

func TestRenewMailboxLeaseExtends(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	mailboxID := createTestMailbox(t, q)

	lease, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 5,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	renewed, err := q.RenewMailboxLease(ctx, store.RenewMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 300,
	})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renew did not extend: %v -> %v", lease.ExpiresAt, renewed.ExpiresAt)
	}

	// Renewal by a non-owner must not touch the lease.
	if _, err := q.RenewMailboxLease(ctx, store.RenewMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-b", TtlSeconds: 300,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("non-owner renew: err = %v, want pgx.ErrNoRows", err)
	}
}

func TestReleaseMailboxLease(t *testing.T) {
	ctx := context.Background()
	q := store.New(testPool)
	mailboxID := createTestMailbox(t, q)

	if _, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a", TtlSeconds: 60,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// A non-owner release is a 0-row no-op.
	n, err := q.ReleaseMailboxLease(ctx, store.ReleaseMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-b",
	})
	if err != nil || n != 0 {
		t.Fatalf("non-owner release: n=%d err=%v, want 0 rows, nil", n, err)
	}

	// Owner release frees the mailbox for immediate adoption.
	n, err = q.ReleaseMailboxLease(ctx, store.ReleaseMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-a",
	})
	if err != nil || n != 1 {
		t.Fatalf("owner release: n=%d err=%v, want 1 row, nil", n, err)
	}
	lease, err := q.ClaimMailboxLease(ctx, store.ClaimMailboxLeaseParams{
		MailboxID: mailboxID, Owner: "worker-b", TtlSeconds: 60,
	})
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if lease.Owner != "worker-b" {
		t.Fatalf("owner after release+claim = %q, want worker-b", lease.Owner)
	}
}
