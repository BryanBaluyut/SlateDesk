package handlers

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/BryanBaluyut/slatedesk/internal/problem"
)

const (
	// sseHeartbeatInterval is how often a comment frame is written to keep
	// intermediaries from reaping idle SSE connections (~25s per the M2
	// spec).
	sseHeartbeatInterval = 25 * time.Second

	// sseWriteTimeout bounds every frame write. A client that cannot accept
	// a few bytes within it is dead or has stopped reading; without the
	// deadline the write blocks forever on a full socket buffer, the
	// goroutine never returns to the select, and the connection is never
	// reclaimed (the request context is not canceled while the socket stays
	// open).
	sseWriteTimeout = 10 * time.Second

	// maxEventStreams caps concurrent SSE subscribers per process so an
	// authenticated but misbehaving client cannot pin unbounded goroutines
	// and hub subscriptions by opening streams it never reads.
	maxEventStreams = 256
)

// StreamEvents implements GET /events (agent/admin): the SSE stream backed
// by the in-process hub (which the LISTEN goroutine feeds from pg_notify).
// Each hub payload — the pg_notify JSON {type, ticket_id} — is forwarded
// verbatim as one `data:` frame. Events are cache-invalidation hints, not a
// durable feed: on (re)connect a client must refetch what it displays.
func (h *Handlers) StreamEvents(w http.ResponseWriter, r *http.Request) {
	// Bounded concurrency: take a stream slot or turn the client away.
	select {
	case h.sseSlots <- struct{}{}:
		defer func() { <-h.sseSlots }()
	default:
		problem.Write(w, r, http.StatusServiceUnavailable, "Service Unavailable", "too many concurrent event streams; retry later")
		return
	}

	rc := http.NewResponseController(w)

	// writeFrame writes one SSE frame under a fresh write deadline, so a
	// stalled client fails the write (and frees this goroutine) instead of
	// blocking on its full socket buffer until the client goes away.
	writeFrame := func(format string, args ...any) error {
		if err := rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
			// Not fatal (e.g. an exotic ResponseWriter wrapper): heartbeat
			// write errors still detect dead clients, just without a bound
			// on a single write.
			slog.Debug("sse: set write deadline", "error", err)
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// Disable proxy response buffering (nginx et al.) so frames flush now.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Opening comment so clients (and intermediaries) see bytes at once.
	if err := writeFrame(": connected\n\n"); err != nil {
		return
	}

	sub, unsubscribe := h.hub.Subscribe()
	defer unsubscribe()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected (or server shutting down): unsubscribe via
			// the defer and free the goroutine.
			return
		case <-heartbeat.C:
			if err := writeFrame(": ping\n\n"); err != nil {
				return
			}
		case payload, ok := <-sub:
			if !ok {
				return
			}
			if err := writeFrame("data: %s\n\n", payload); err != nil {
				return
			}
		}
	}
}
