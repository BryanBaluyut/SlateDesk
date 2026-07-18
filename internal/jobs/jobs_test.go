package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/BryanBaluyut/slatedesk/internal/jobs"
)

// testArgs is a trivial job for the boot smoke test.
type testArgs struct {
	Msg string `json:"msg"`
}

func (testArgs) Kind() string { return "test_noop" }

// orphanArgs is a kind no worker in this package handles; used by the
// insert-only test so its job can never be picked up by another test's
// running worker (tests share one database).
type orphanArgs struct{}

func (orphanArgs) Kind() string { return "test_orphan" }

// testWorker reports each worked job's Msg on done.
type testWorker struct {
	river.WorkerDefaults[testArgs]
	done chan string
}

func (w *testWorker) Work(_ context.Context, job *river.Job[testArgs]) error {
	w.done <- job.Args.Msg
	return nil
}

// TestRiverBootSmoke boots a worker client on the migrated throwaway
// database, inserts trivial jobs — one plain, one transactionally, one on
// the email queue — and sees all three processed.
func TestRiverBootSmoke(t *testing.T) {
	ctx := context.Background()

	done := make(chan string, 8)
	reg := jobs.NewRegistry()
	if err := jobs.AddWorker(reg, &testWorker{done: done}); err != nil {
		t.Fatalf("register test worker: %v", err)
	}
	if err := reg.RegisterBuiltins(jobs.Deps{Pool: testPool}); err != nil {
		t.Fatalf("register builtins: %v", err)
	}

	client, err := jobs.NewClient(testPool, reg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("stop client: %v", err)
		}
	}()

	// Plain insert on the default queue.
	if _, err := client.River().Insert(ctx, testArgs{Msg: "plain"}, nil); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Transactional insert — the property River was chosen for: the job
	// becomes visible iff the surrounding transaction commits.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := client.River().InsertTx(ctx, tx, testArgs{Msg: "tx"}, nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Email queue is configured and works jobs too.
	if _, err := client.River().Insert(ctx, testArgs{Msg: "email-queue"},
		&river.InsertOpts{Queue: jobs.QueueEmail}); err != nil {
		t.Fatalf("insert on email queue: %v", err)
	}

	want := map[string]bool{"plain": true, "tx": true, "email-queue": true}
	deadline := time.After(30 * time.Second)
	for len(want) > 0 {
		select {
		case msg := <-done:
			if !want[msg] {
				t.Fatalf("unexpected or duplicate job worked: %q", msg)
			}
			delete(want, msg)
		case <-deadline:
			t.Fatalf("timed out; unworked jobs: %v", want)
		}
	}
}

// TestRolledBackInsertNeverRuns pins the flip side of transactional
// enqueue: a job inserted in a rolled-back transaction must never surface.
func TestRolledBackInsertNeverRuns(t *testing.T) {
	ctx := context.Background()

	done := make(chan string, 8)
	reg := jobs.NewRegistry()
	if err := jobs.AddWorker(reg, &testWorker{done: done}); err != nil {
		t.Fatalf("register test worker: %v", err)
	}
	client, err := jobs.NewClient(testPool, reg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.Stop(stopCtx); err != nil {
			t.Errorf("stop client: %v", err)
		}
	}()

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := client.River().InsertTx(ctx, tx, testArgs{Msg: "ghost"}, nil); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	select {
	case msg := <-done:
		t.Fatalf("job from rolled-back tx was worked: %q", msg)
	case <-time.After(3 * time.Second):
		// Nothing surfaced: correct.
	}
}

// TestInsertOnlyClient covers the `serve --no-worker` role: enqueue works,
// Start/Stop are safe no-ops.
func TestInsertOnlyClient(t *testing.T) {
	ctx := context.Background()

	client, err := jobs.NewClient(testPool, nil)
	if err != nil {
		t.Fatalf("new insert-only client: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("insert-only Start should be a no-op, got: %v", err)
	}
	if _, err := client.River().Insert(ctx, orphanArgs{}, nil); err != nil {
		t.Fatalf("insert-only enqueue: %v", err)
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("insert-only Stop should be a no-op, got: %v", err)
	}
}
