package events

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Listener holds one dedicated Postgres connection in LISTEN mode and
// republishes every notification on Channel into the Hub. It reconnects
// with capped exponential backoff, so a database restart only pauses
// realtime updates instead of killing them.
//
// It uses its own pgx.Conn (not the app pool): LISTEN is session state, and
// a pooled connection could be recycled out from under it.
type Listener struct {
	databaseURL string
	hub         *Hub

	// connect is swappable for tests.
	connect func(ctx context.Context, url string) (*pgx.Conn, error)
}

// NewListener returns a Listener publishing into hub. Call Run to start it.
func NewListener(databaseURL string, hub *Hub) *Listener {
	return &Listener{
		databaseURL: databaseURL,
		hub:         hub,
		connect:     pgx.Connect,
	}
}

const (
	listenBackoffMin = 250 * time.Millisecond
	listenBackoffMax = 15 * time.Second
)

// Run blocks, maintaining the LISTEN connection until ctx is canceled.
// It always returns nil after a clean shutdown; connection errors are
// logged and retried, never returned.
func (l *Listener) Run(ctx context.Context) error {
	backoff := listenBackoffMin
	for {
		if err := l.listenOnce(ctx, &backoff); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Warn("events: listener connection lost, reconnecting", "error", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, listenBackoffMax)
	}
}

// listenOnce dials, LISTENs, and pumps notifications until the connection
// or ctx dies. On the first successful notification wait the caller's
// backoff is reset.
func (l *Listener) listenOnce(ctx context.Context, backoff *time.Duration) error {
	conn, err := l.connect(ctx, l.databaseURL)
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	slog.Info("events: listening", "channel", Channel)

	// Notifications fired while we were NOT listening are gone (pg_notify
	// is not durable), yet connected SSE clients kept receiving heartbeats
	// and have no idea. Tell them to refetch everything they display; on
	// the very first connect this is a harmless no-op (nobody is
	// subscribed yet).
	l.hub.Publish([]byte(`{"type":"resync"}`))

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		// A healthy notification proves the connection is good: reset the
		// reconnect backoff.
		*backoff = listenBackoffMin
		l.hub.Publish([]byte(n.Payload))
	}
}
