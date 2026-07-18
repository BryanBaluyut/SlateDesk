// Package events is the M2 realtime spine (architecture doc §4 Realtime):
// services call pg_notify inside their write transaction (so a notification
// fires only on commit), a per-process Listener holds one dedicated LISTEN
// connection and republishes payloads into an in-process Hub, and the SSE
// handler streams Hub events to connected agent browsers. Any replica can
// serve any client because fan-out goes through Postgres.
//
// A durable outbox (guaranteed delivery for webhooks) is deliberately
// deferred to M4; SSE clients only use events as a cache-invalidation hint,
// so a dropped notification merely delays a refresh.
package events

import (
	"sync"
)

// Channel is the Postgres NOTIFY channel all SlateDesk events travel on.
// Payloads are small JSON documents like {"type":"ticket.updated",
// "ticket_id":"<uuid>"} (see the ticket service).
const Channel = "slatedesk_events"

// subscriberBuffer is each subscriber's buffered-channel capacity.
const subscriberBuffer = 32

// Hub fans events out to in-process subscribers (SSE connections).
//
// Delivery policy — drop-slowest: Publish never blocks. Each subscriber has
// a buffered channel; if a subscriber's buffer is full (a slow or stalled
// client), the event is dropped for that subscriber only, and everyone else
// still receives it. This is safe because events are invalidation hints,
// not a source of truth — a client that missed one refetches on its next
// event or reconnect.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan []byte]struct{})}
}

// Subscribe registers a new subscriber and returns its receive channel plus
// an unsubscribe func. Unsubscribe is idempotent and closes the channel.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, subscriberBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
	return ch, unsubscribe
}

// Publish delivers payload to every subscriber, dropping it for any whose
// buffer is full (see the drop-slowest policy above). It never blocks.
func (h *Hub) Publish(payload []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- payload:
		default:
			// Subscriber buffer full: drop for this subscriber only.
		}
	}
}

// SubscriberCount reports the number of active subscribers (used by tests
// and, later, health/metrics).
func (h *Hub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
