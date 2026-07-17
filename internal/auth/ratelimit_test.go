package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	type step struct {
		key     string
		advance time.Duration // clock advance before this attempt
		want    bool
	}
	tests := []struct {
		name   string
		burst  int
		refill time.Duration
		steps  []step
	}{
		{
			name:   "burst then deny",
			burst:  3,
			refill: time.Minute,
			steps: []step{
				{key: "a", want: true},
				{key: "a", want: true},
				{key: "a", want: true},
				{key: "a", want: false},
				{key: "a", want: false},
			},
		},
		{
			name:   "keys are independent",
			burst:  1,
			refill: time.Minute,
			steps: []step{
				{key: "a", want: true},
				{key: "a", want: false},
				{key: "b", want: true},
				{key: "b", want: false},
			},
		},
		{
			name:   "refill grants one attempt per interval",
			burst:  2,
			refill: 30 * time.Second,
			steps: []step{
				{key: "a", want: true},
				{key: "a", want: true},
				{key: "a", want: false},
				{key: "a", advance: 30 * time.Second, want: true},
				{key: "a", want: false},
				{key: "a", advance: 15 * time.Second, want: false},
				{key: "a", advance: 15 * time.Second, want: true},
			},
		},
		{
			name:   "refill caps at burst",
			burst:  2,
			refill: time.Second,
			steps: []step{
				{key: "a", want: true},
				// A long idle period must not bank more than burst tokens.
				{key: "a", advance: time.Hour, want: true},
				{key: "a", want: true},
				{key: "a", want: false},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewRateLimiter(tt.burst, tt.refill)
			now := time.Unix(1_700_000_000, 0)
			l.now = func() time.Time { return now }
			for i, s := range tt.steps {
				now = now.Add(s.advance)
				if got := l.Allow(s.key); got != s.want {
					t.Fatalf("step %d (key %q): Allow() = %v, want %v", i, s.key, got, s.want)
				}
			}
		})
	}
}

func TestRateLimiterPrunesStaleBuckets(t *testing.T) {
	l := NewRateLimiter(2, time.Second)
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 10_001; i++ {
		l.Allow(fmt.Sprintf("key-%d", i))
	}
	if len(l.buckets) <= 10_000 {
		t.Fatalf("precondition: expected >10000 buckets, got %d", len(l.buckets))
	}
	// After every bucket has fully refilled, the next new key triggers a
	// sweep of stale entries.
	now = now.Add(time.Hour)
	l.Allow("fresh-key")
	if len(l.buckets) > 2 {
		t.Fatalf("expected stale buckets pruned, still have %d", len(l.buckets))
	}
}
