// Package breaker implements a small circuit breaker with the classic
// closed / open / half-open state machine.
//
// A Breaker starts Closed and lets every request through. After a configured
// number of consecutive failures it trips Open and rejects requests until a
// cooldown elapses. It then enters HalfOpen and admits a single probe
// request: a success closes the circuit, a failure re-opens it.
//
// The clock is injectable so the state machine can be unit tested without
// sleeping.
package breaker

import (
	"sync"
	"time"
)

// State is the current position of the breaker's state machine.
type State int

// Breaker states. The numeric values are stable and are exported as a metric
// gauge: 0 closed, 1 open, 2 half-open.
const (
	Closed State = iota
	Open
	HalfOpen
)

// String returns the lowercase name of the state.
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Option customises a Breaker.
type Option func(*Breaker)

// WithClock replaces the time source. Useful in tests.
func WithClock(now func() time.Time) Option {
	return func(b *Breaker) { b.now = now }
}

// WithOnStateChange registers a callback invoked on every transition. The
// callback runs while the breaker's lock is held, so it must not call back
// into the breaker.
func WithOnStateChange(fn func(from, to State)) Option {
	return func(b *Breaker) { b.onChange = fn }
}

// Breaker is a concurrency-safe circuit breaker.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	onChange  func(from, to State)

	mu       sync.Mutex
	state    State
	failures int
	openedAt time.Time
	probing  bool
}

// New returns a Closed breaker that trips after threshold consecutive
// failures and stays Open for cooldown. A threshold below 1 is treated as 1.
func New(threshold int, cooldown time.Duration, opts ...Option) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	b := &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Allow reports whether a request may proceed. When it returns true the
// caller must later report the outcome with Success or Failure.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.transition(HalfOpen)
		b.probing = true
		return true
	default: // HalfOpen: admit exactly one probe at a time.
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// Success records a successful request.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		b.failures = 0
	case HalfOpen:
		b.probing = false
		b.failures = 0
		b.transition(Closed)
	}
}

// Failure records a failed request.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	case HalfOpen:
		b.probing = false
		b.trip()
	}
}

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Failures returns the number of consecutive failures seen while Closed.
func (b *Breaker) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

func (b *Breaker) trip() {
	b.openedAt = b.now()
	b.failures = 0
	b.transition(Open)
}

func (b *Breaker) transition(to State) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	if b.onChange != nil {
		b.onChange(from, to)
	}
}
