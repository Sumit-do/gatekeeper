// Package ratelimit implements a keyed token-bucket rate limiter.
//
// Every distinct key (an API key or a client IP, for example) owns a bucket
// that holds up to burst tokens and refills continuously at rps tokens per
// second. A request consumes one token; when none is available the limiter
// reports how long the caller should wait. Idle buckets are evicted so that
// memory stays proportional to the number of recently active clients.
package ratelimit

import (
	"context"
	"math"
	"sync"
	"time"
)

// Option customises a Limiter.
type Option func(*Limiter)

// WithClock replaces the time source. Useful in tests.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) { l.now = now }
}

// WithIdleTimeout sets how long a bucket may go unused before Evict removes
// it. The default is five minutes.
func WithIdleTimeout(d time.Duration) Option {
	return func(l *Limiter) { l.idle = d }
}

// Limiter is a concurrency-safe collection of token buckets keyed by client.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	idle  time.Duration
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing rps sustained requests per second with the
// given burst capacity per key. Non-positive inputs are clamped to sane
// minimums so the limiter never divides by zero.
func New(rps float64, burst int, opts ...Option) *Limiter {
	if rps <= 0 || math.IsNaN(rps) || math.IsInf(rps, 0) {
		rps = 1
	}
	if burst < 1 {
		burst = 1
	}
	l := &Limiter{
		rate:    rps,
		burst:   float64(burst),
		idle:    5 * time.Minute,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Allow consumes one token from key's bucket. It returns true when the
// request may proceed. Otherwise it returns false along with the duration
// after which one token will be available.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		b.refill(now, l.rate, l.burst)
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// refill adds the tokens accrued since the last update, capped at burst.
func (b *bucket) refill(now time.Time, rate, burst float64) {
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	b.tokens = math.Min(burst, b.tokens+elapsed.Seconds()*rate)
	b.last = now
}

// Evict removes buckets that have not been touched for the idle timeout and
// returns how many were removed.
func (l *Limiter) Evict() int {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	n := 0
	for key, b := range l.buckets {
		if now.Sub(b.last) >= l.idle {
			delete(l.buckets, key)
			n++
		}
	}
	return n
}

// Len returns the number of live buckets.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Run calls Evict every interval until ctx is cancelled. It is intended to be
// started in its own goroutine.
func (l *Limiter) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.Evict()
		}
	}
}
