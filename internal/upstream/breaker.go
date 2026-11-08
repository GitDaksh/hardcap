// Package upstream holds the rules for talking to AI providers that fail:
// circuit breakers, which failures are safe to retry, and how long to wait.
package upstream

import (
	"sync"
	"sync/atomic"
	"time"
)

// State is a circuit breaker's state.
type State int

const (
	// Closed: the provider looks healthy and gets traffic.
	Closed State = iota
	// HalfOpen: the cooldown is over and one trial request is allowed through.
	HalfOpen
	// Open: the provider failed repeatedly and is skipped until the cooldown ends.
	Open
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case HalfOpen:
		return "half-open"
	default:
		return "open"
	}
}

// Breaker stops sending traffic to a provider that keeps failing, so requests
// fall back to the next provider straight away instead of each one waiting
// for the same failure. After a cooldown, one trial request decides whether
// the provider is back.
//
// Each gateway instance keeps its own breakers in memory. That's fine:
// breakers are an optimisation, and each instance learns quickly on its own.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	onChange  func(State)

	mu       sync.Mutex
	state    State
	failures int // in a row, while closed
	openedAt time.Time
	probing  bool // a half-open trial is in flight
}

// NewBreaker returns a closed breaker that opens after threshold failures in
// a row. onChange (optional) is called on every state change, e.g. to update
// a metric.
func NewBreaker(threshold int, cooldown time.Duration, onChange func(State)) *Breaker {
	return &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now, onChange: onChange}
}

// SetClock replaces the breaker's clock (for tests).
func (b *Breaker) SetClock(now func() time.Time) { b.now = now }

// State returns the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a request may go to the provider now. When it may,
// the caller must call done once with whether the provider worked (calls
// after the first are ignored).
func (b *Breaker) Allow() (done func(success bool), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return b.reporter(false), true
	case Open:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return nil, false
		}
		b.setState(HalfOpen)
	}
	// Half-open: exactly one trial request at a time.
	if b.probing {
		return nil, false
	}
	b.probing = true
	return b.reporter(true), true
}

func (b *Breaker) reporter(probe bool) func(bool) {
	var called atomic.Bool
	return func(success bool) {
		if !called.Swap(true) {
			b.record(probe, success)
		}
	}
}

func (b *Breaker) record(probe, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
	}
	switch {
	case b.state == Closed:
		if success {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	case probe:
		if success {
			b.failures = 0
			b.setState(Closed)
		} else {
			b.trip()
		}
	default:
		// A request that started before the breaker opened finished late.
		// Its result is old news, so it changes nothing.
	}
}

func (b *Breaker) trip() {
	b.openedAt = b.now()
	b.setState(Open)
}

func (b *Breaker) setState(s State) {
	if b.state == s {
		return
	}
	b.state = s
	if b.onChange != nil {
		b.onChange(s)
	}
}
