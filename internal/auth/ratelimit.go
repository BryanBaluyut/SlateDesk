package auth

import (
	"sync"
	"time"
)

// RateLimiter is a simple in-memory token-bucket limiter keyed by an opaque
// string (the login handler uses "ip|email"). Good enough for M1: state is
// per-process, so N replicas each allow their own budget — acceptable for a
// login brake, not a billing meter.
type RateLimiter struct {
	burst  float64       // bucket capacity (max tokens)
	refill time.Duration // time to regain one token
	now    func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter returns a limiter allowing `burst` immediate attempts per
// key, refilling one attempt every `refill`.
func NewRateLimiter(burst int, refill time.Duration) *RateLimiter {
	return &RateLimiter{
		burst:   float64(burst),
		refill:  refill,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// Allow reports whether one attempt for key may proceed, consuming a token
// if so.
func (l *RateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		// Opportunistic cleanup so abandoned keys do not accumulate
		// forever: drop any bucket that has fully refilled.
		if len(l.buckets) > 10_000 {
			for k, old := range l.buckets {
				if now.Sub(old.last) >= time.Duration(l.burst)*l.refill {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Refill for the elapsed time, capped at capacity.
	elapsed := now.Sub(b.last)
	b.tokens += float64(elapsed) / float64(l.refill)
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
