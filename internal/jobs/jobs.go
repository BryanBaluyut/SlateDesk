// Package jobs wires River — the Postgres-native job queue — into
// SlateDesk.
//
// River was chosen for exactly one property (architecture doc §1):
// transactional enqueue. An outbound article INSERT and its email_send job
// commit atomically via Client.River().InsertTx on the same pgx.Tx, so a
// reply can never be saved without its email durably queued.
//
// One binary, three roles (architecture doc §4): `slatedesk serve` runs
// embedded workers, `serve --no-worker` is insert-only, and
// `slatedesk worker` runs workers without HTTP. All three use this
// package; the difference is whether a Registry is supplied and Start is
// called.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Queue names. Jobs default to QueueDefault; email delivery runs on its
// own queue so a flood of notifications can never starve ticket mail (and
// vice versa).
const (
	QueueDefault = river.QueueDefault
	QueueEmail   = "email"
)

// Per-queue parallelism within one worker process.
const (
	defaultQueueMaxWorkers = 10
	emailQueueMaxWorkers   = 5
)

// stopTimeout bounds the graceful phase of Client.Stop before escalating
// to cancellation of in-flight jobs.
const stopTimeout = 15 * time.Second

// Deps carries what job workers need to do their work. The M3 email
// engine phase grows this (mailer, secrets box, blob storage, ...) as its
// workers land.
type Deps struct {
	Pool *pgxpool.Pool
}

// Registry collects the River workers a process can run, and knows how
// many were registered. Build one with NewRegistry, fill it with
// RegisterBuiltins (plus AddWorker in tests), then hand it to NewClient.
type Registry struct {
	workers *river.Workers
	count   int
}

// NewRegistry returns an empty worker registry.
func NewRegistry() *Registry {
	return &Registry{workers: river.NewWorkers()}
}

// AddWorker registers a single worker. A free function rather than a
// method because Go methods cannot introduce type parameters.
func AddWorker[T river.JobArgs](r *Registry, worker river.Worker[T]) error {
	if err := river.AddWorkerSafely(r.workers, worker); err != nil {
		return fmt.Errorf("jobs: register worker: %w", err)
	}
	r.count++
	return nil
}

// RegisterBuiltins registers every built-in SlateDesk worker.
//
// M3 spine: none exist yet. The email engine phase registers its workers
// here (email_send, ticket auto-ack, notify-assignee) so serve and worker
// modes pick them up with no further wiring.
func (r *Registry) RegisterBuiltins(deps Deps) error {
	_ = deps // used once the first builtin worker lands
	return nil
}

// Len reports how many workers are registered.
func (r *Registry) Len() int { return r.count }

// Client wraps the River client plus the start/stop lifecycle for the
// binary's three roles.
type Client struct {
	river       *river.Client[pgx.Tx]
	workerCount int
	started     bool
}

// NewClient constructs the River client on the application's pgx pool
// (riverpgxv5 driver — jobs are rows in the same database, and enqueues
// can join application transactions).
//
// reg == nil produces an insert-only client (`serve --no-worker`): it can
// enqueue but never fetches or works jobs, and Start is a logged no-op.
func NewClient(pool *pgxpool.Pool, reg *Registry) (*Client, error) {
	cfg := &river.Config{
		Logger: slog.Default(),
	}
	workerCount := 0
	if reg != nil {
		workerCount = reg.Len()
		cfg.Workers = reg.workers
		cfg.Queues = map[string]river.QueueConfig{
			QueueDefault: {MaxWorkers: defaultQueueMaxWorkers},
			QueueEmail:   {MaxWorkers: emailQueueMaxWorkers},
		}
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("jobs: create river client: %w", err)
	}
	return &Client{river: rc, workerCount: workerCount}, nil
}

// River exposes the underlying client for enqueueing — in particular
// InsertTx, which is how services make job enqueue atomic with their own
// writes.
func (c *Client) River() *river.Client[pgx.Tx] { return c.river }

// Start begins fetching and working jobs. The context should outlive
// shutdown signals (e.g. context.WithoutCancel of the signal context):
// cancelling it hard-stops in-flight jobs, whereas Stop drains gracefully.
//
// While the worker registry is empty (M3 spine before the engine phase
// lands workers) Start logs and does nothing, because River refuses to
// start a client with zero registered workers.
func (c *Client) Start(ctx context.Context) error {
	if c.workerCount == 0 {
		slog.Info("river: no job workers registered yet; not starting worker loops")
		return nil
	}
	if err := c.river.Start(ctx); err != nil {
		return fmt.Errorf("jobs: start river client: %w", err)
	}
	c.started = true
	slog.Info("river: workers started", "workers", c.workerCount,
		"queues", []string{QueueDefault, QueueEmail})
	return nil
}

// Stop shuts the client down: graceful drain first (in-flight jobs finish,
// bounded by stopTimeout and ctx), then escalation to StopAndCancel. Safe
// to call on a client that never started.
func (c *Client) Stop(ctx context.Context) error {
	if !c.started {
		return nil
	}
	c.started = false

	softCtx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	err := c.river.Stop(softCtx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("jobs: stop river client: %w", err)
	}

	slog.Warn("river: graceful stop timed out; cancelling in-flight jobs")
	if err := c.river.StopAndCancel(ctx); err != nil {
		return fmt.Errorf("jobs: hard-stop river client: %w", err)
	}
	return nil
}
