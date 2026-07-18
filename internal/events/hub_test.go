package events

import (
	"fmt"
	"testing"
)

func TestHubFanOut(t *testing.T) {
	h := NewHub()
	a, cancelA := h.Subscribe()
	b, cancelB := h.Subscribe()
	defer cancelA()
	defer cancelB()

	h.Publish([]byte("one"))
	if got := string(<-a); got != "one" {
		t.Fatalf("subscriber a got %q; want one", got)
	}
	if got := string(<-b); got != "one" {
		t.Fatalf("subscriber b got %q; want one", got)
	}

	cancelB()
	if n := h.SubscriberCount(); n != 1 {
		t.Fatalf("SubscriberCount = %d after unsubscribe; want 1", n)
	}
	// Unsubscribe is idempotent and Publish still reaches a.
	cancelB()
	h.Publish([]byte("two"))
	if got := string(<-a); got != "two" {
		t.Fatalf("subscriber a got %q; want two", got)
	}
}

func TestHubDropsForSlowSubscriberOnly(t *testing.T) {
	h := NewHub()
	slow, cancelSlow := h.Subscribe()
	fast, cancelFast := h.Subscribe()
	defer cancelSlow()
	defer cancelFast()

	// Overfill the slow subscriber's buffer without reading; drain fast
	// as we go. Publish must never block and fast must see everything.
	total := subscriberBuffer + 10
	for i := range total {
		h.Publish(fmt.Appendf(nil, "evt-%d", i))
		if got := string(<-fast); got != fmt.Sprintf("evt-%d", i) {
			t.Fatalf("fast subscriber got %q at %d", got, i)
		}
	}

	// Slow still holds exactly its buffer worth; the overflow was dropped.
	if got := len(slow); got != subscriberBuffer {
		t.Fatalf("slow buffer holds %d events; want %d", got, subscriberBuffer)
	}
	for i := range subscriberBuffer {
		if got := string(<-slow); got != fmt.Sprintf("evt-%d", i) {
			t.Fatalf("slow subscriber got %q at %d; want in-order prefix", got, i)
		}
	}
}
