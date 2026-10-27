package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestBurstThenDeny(t *testing.T) {
	clk := newClock()
	l := New(1, 3, WithClock(clk.Now))

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d within burst must be allowed", i+1)
		}
	}
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("request beyond burst must be denied")
	}
	if retry != time.Second {
		t.Fatalf("retry-after = %s, want 1s at 1 rps with an empty bucket", retry)
	}
}

func TestRefillMath(t *testing.T) {
	clk := newClock()
	l := New(2, 4, WithClock(clk.Now)) // 2 tokens/s, cap 4

	for i := 0; i < 4; i++ {
		l.Allow("k")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("bucket should be empty")
	}

	clk.Advance(500 * time.Millisecond) // +1 token
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("one token should have refilled after 500ms at 2 rps")
	}
	if ok, retry := l.Allow("k"); ok {
		t.Fatal("second token should not be available yet")
	} else if retry != 500*time.Millisecond {
		t.Fatalf("retry-after = %s, want 500ms", retry)
	}

	clk.Advance(time.Hour) // refill is capped at burst
	for i := 0; i < 4; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d after long idle must be allowed (burst cap)", i+1)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("refill must be capped at burst")
	}
}

func TestPartialTokenRetryAfter(t *testing.T) {
	clk := newClock()
	l := New(1, 1, WithClock(clk.Now))
	l.Allow("k")
	clk.Advance(250 * time.Millisecond) // 0.25 tokens
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("0.25 tokens must not admit a request")
	}
	if retry != 750*time.Millisecond {
		t.Fatalf("retry-after = %s, want 750ms", retry)
	}
}

func TestKeysAreIndependent(t *testing.T) {
	l := New(1, 1, WithClock(newClock().Now))
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a: first request must pass")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a: second request must be denied")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b must have its own bucket")
	}
	if l.Len() != 2 {
		t.Fatalf("Len = %d, want 2", l.Len())
	}
}

func TestEvictIdleBuckets(t *testing.T) {
	clk := newClock()
	l := New(10, 10, WithClock(clk.Now), WithIdleTimeout(time.Minute))

	l.Allow("old")
	clk.Advance(30 * time.Second)
	l.Allow("fresh")

	if n := l.Evict(); n != 0 {
		t.Fatalf("evicted %d before idle timeout, want 0", n)
	}
	clk.Advance(30 * time.Second) // "old" idle 60s, "fresh" idle 30s
	if n := l.Evict(); n != 1 {
		t.Fatalf("evicted %d, want 1", n)
	}
	if l.Len() != 1 {
		t.Fatalf("Len = %d, want 1", l.Len())
	}
	clk.Advance(time.Minute)
	if n := l.Evict(); n != 1 || l.Len() != 0 {
		t.Fatalf("evicted %d (len %d), want 1 (len 0)", n, l.Len())
	}

	// An evicted key starts over with a full bucket.
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow("old"); !ok {
			t.Fatalf("recreated bucket must start full (failed at %d)", i+1)
		}
	}
}

func TestRunEvictsPeriodically(t *testing.T) {
	clk := newClock()
	l := New(1, 1, WithClock(clk.Now), WithIdleTimeout(time.Millisecond))
	l.Allow("k")
	clk.Advance(time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx, time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for l.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if l.Len() != 0 {
		t.Fatal("Run did not evict the idle bucket")
	}
}

func TestInvalidInputsClamped(t *testing.T) {
	l := New(0, 0)
	if l.rate != 1 || l.burst != 1 {
		t.Fatalf("rate=%v burst=%v, want 1/1", l.rate, l.burst)
	}
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("clamped limiter must still allow one request")
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := New(1000, 100, WithClock(newClock().Now))
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("shared"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 100 {
		t.Fatalf("allowed %d concurrent requests, want exactly burst (100)", allowed)
	}
}
