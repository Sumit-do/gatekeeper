package breaker

import (
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

func TestStartsClosedAndAllows(t *testing.T) {
	b := New(3, time.Minute)
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed", b.State())
	}
	for i := 0; i < 10; i++ {
		if !b.Allow() {
			t.Fatal("closed breaker must allow")
		}
	}
}

func TestOpensAfterThresholdFailures(t *testing.T) {
	clk := newClock()
	b := New(3, time.Minute, WithClock(clk.Now))

	b.Failure()
	b.Failure()
	if b.State() != Closed || b.Failures() != 2 {
		t.Fatalf("after 2 failures: state=%s failures=%d", b.State(), b.Failures())
	}
	b.Failure()
	if b.State() != Open {
		t.Fatalf("after 3 failures: state=%s, want open", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker must reject before cooldown")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b := New(3, time.Minute)
	b.Failure()
	b.Failure()
	b.Success()
	if b.Failures() != 0 {
		t.Fatalf("failures = %d, want 0", b.Failures())
	}
	b.Failure()
	b.Failure()
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed (non-consecutive failures)", b.State())
	}
}

func TestHalfOpenAdmitsSingleProbe(t *testing.T) {
	clk := newClock()
	b := New(1, 30*time.Second, WithClock(clk.Now))
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %s, want open", b.State())
	}

	clk.Advance(29 * time.Second)
	if b.Allow() {
		t.Fatal("must stay open until cooldown elapses")
	}

	clk.Advance(time.Second)
	if !b.Allow() {
		t.Fatal("first request after cooldown must be admitted as probe")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state = %s, want half-open", b.State())
	}
	if b.Allow() {
		t.Fatal("second concurrent probe must be rejected")
	}
}

func TestHalfOpenSuccessCloses(t *testing.T) {
	clk := newClock()
	b := New(1, time.Second, WithClock(clk.Now))
	b.Failure()
	clk.Advance(time.Second)
	if !b.Allow() {
		t.Fatal("probe not admitted")
	}
	b.Success()
	if b.State() != Closed {
		t.Fatalf("state = %s, want closed", b.State())
	}
	if !b.Allow() || !b.Allow() {
		t.Fatal("closed breaker must allow freely")
	}
}

func TestHalfOpenFailureReopensWithFreshCooldown(t *testing.T) {
	clk := newClock()
	b := New(1, 10*time.Second, WithClock(clk.Now))
	b.Failure()
	clk.Advance(10 * time.Second)
	if !b.Allow() {
		t.Fatal("probe not admitted")
	}
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %s, want open", b.State())
	}
	clk.Advance(5 * time.Second)
	if b.Allow() {
		t.Fatal("cooldown must restart after a failed probe")
	}
	clk.Advance(5 * time.Second)
	if !b.Allow() {
		t.Fatal("probe must be admitted after full cooldown")
	}
}

func TestOnStateChangeCallback(t *testing.T) {
	clk := newClock()
	var transitions []string
	b := New(1, time.Second, WithClock(clk.Now), WithOnStateChange(func(from, to State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	}))
	b.Failure()
	clk.Advance(time.Second)
	b.Allow()
	b.Success()

	want := []string{"closed->open", "open->half-open", "half-open->closed"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	}
}

func TestThresholdClampedToOne(t *testing.T) {
	b := New(0, time.Second)
	b.Failure()
	if b.State() != Open {
		t.Fatalf("state = %s, want open with threshold clamped to 1", b.State())
	}
}

func TestStateString(t *testing.T) {
	if Closed.String() != "closed" || Open.String() != "open" || HalfOpen.String() != "half-open" || State(99).String() != "unknown" {
		t.Fatal("unexpected State.String output")
	}
}

func TestConcurrentAccess(t *testing.T) {
	b := New(5, time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if b.Allow() {
				if i%3 == 0 {
					b.Failure()
				} else {
					b.Success()
				}
			}
		}(i)
	}
	wg.Wait()
	_ = b.State()
}
