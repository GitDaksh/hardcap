package upstream_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/upstream"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newBreaker(changes *[]upstream.State) (*upstream.Breaker, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	b := upstream.NewBreaker(3, 30*time.Second, func(s upstream.State) { *changes = append(*changes, s) })
	b.SetClock(clock.now)
	return b, clock
}

func call(t *testing.T, b *upstream.Breaker, success bool) {
	t.Helper()
	done, ok := b.Allow()
	if !ok {
		t.Fatal("breaker refused a request it should allow")
	}
	done(success)
}

func TestBreakerOpensAfterFailuresInARow(t *testing.T) {
	var changes []upstream.State
	b, _ := newBreaker(&changes)

	call(t, b, false)
	call(t, b, false)
	call(t, b, true) // a success resets the count
	call(t, b, false)
	call(t, b, false)
	if b.State() != upstream.Closed {
		t.Fatal("2 failures in a row shouldn't open it")
	}
	call(t, b, false)
	if b.State() != upstream.Open {
		t.Fatal("3 failures in a row should open it")
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("an open breaker must refuse requests")
	}
	if len(changes) != 1 || changes[0] != upstream.Open {
		t.Errorf("changes = %v", changes)
	}
}

func TestBreakerTrialRequestAfterCooldown(t *testing.T) {
	var changes []upstream.State
	b, clock := newBreaker(&changes)
	for range 3 {
		call(t, b, false)
	}

	clock.advance(29 * time.Second)
	if _, ok := b.Allow(); ok {
		t.Fatal("still cooling down")
	}
	clock.advance(time.Second)
	probe, ok := b.Allow()
	if !ok || b.State() != upstream.HalfOpen {
		t.Fatal("after the cooldown, one trial request should get through")
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("only ONE trial at a time")
	}

	probe(false) // the trial failed: open again, for a fresh cooldown
	if b.State() != upstream.Open {
		t.Fatal("a failed trial should reopen the breaker")
	}
	clock.advance(30 * time.Second)
	probe, _ = b.Allow()
	probe(true)
	if b.State() != upstream.Closed {
		t.Fatal("a successful trial should close the breaker")
	}
	want := []upstream.State{upstream.Open, upstream.HalfOpen, upstream.Open, upstream.HalfOpen, upstream.Closed}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Fatalf("changes = %v, want %v", changes, want)
		}
	}
}

func TestLateResultsDontReopenOrCloseTheBreaker(t *testing.T) {
	var changes []upstream.State
	b, _ := newBreaker(&changes)
	slow, _ := b.Allow() // starts before the breaker opens
	for range 3 {
		call(t, b, false)
	}
	slow(true) // finishes after: must not close the open breaker
	if b.State() != upstream.Open {
		t.Fatal("a stale success closed an open breaker")
	}
	slow(false) // and calling done twice is ignored
}

func TestClassify(t *testing.T) {
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	cases := []struct {
		name string
		resp *http.Response
		err  error
		want upstream.Verdict
	}{
		{"200", resp(200), nil, upstream.Success},
		{"400 bad request", resp(400), nil, upstream.ClientError},
		{"401 bad key", resp(401), nil, upstream.ClientError},
		{"429 rate limited", resp(429), nil, upstream.Retryable},
		{"500", resp(500), nil, upstream.Retryable},
		{"503", resp(503), nil, upstream.Retryable},
		{"timeout", nil, context.DeadlineExceeded, upstream.Ambiguous},
		{"dropped mid-request", nil, errors.New("unexpected EOF"), upstream.Ambiguous},
		{"dns", nil, &net.DNSError{Err: "no such host", Name: "x"}, upstream.Retryable},
	}
	for _, c := range cases {
		if got := upstream.Classify(c.resp, c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// A real connection refused error.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	_, err := http.Get(url)
	if got := upstream.Classify(nil, err); got != upstream.Retryable {
		t.Errorf("connection refused: got %v, want retryable (%v)", got, err)
	}
}

func TestBackoffStaysWithinItsCeiling(t *testing.T) {
	base, ceiling := 100*time.Millisecond, time.Second
	for attempt := 1; attempt <= 10; attempt++ {
		limit := min(base<<(attempt-1), ceiling)
		for range 200 {
			if d := upstream.Backoff(attempt, base, ceiling); d <= 0 || d > limit {
				t.Fatalf("Backoff(%d) = %v, want within (0, %v]", attempt, d, limit)
			}
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	if _, ok := upstream.RetryAfter(h, now); ok {
		t.Error("no header should mean no value")
	}
	h.Set("Retry-After", "2")
	if d, ok := upstream.RetryAfter(h, now); !ok || d != 2*time.Second {
		t.Errorf("seconds: %v %v", d, ok)
	}
	h.Set("Retry-After", "0.5")
	if d, ok := upstream.RetryAfter(h, now); !ok || d != 500*time.Millisecond {
		t.Errorf("fractional seconds: %v %v", d, ok)
	}
	h.Set("Retry-After", now.Add(3*time.Second).Format(http.TimeFormat))
	if d, ok := upstream.RetryAfter(h, now); !ok || d != 3*time.Second {
		t.Errorf("date: %v %v", d, ok)
	}
	h.Set("Retry-After", "soon")
	if _, ok := upstream.RetryAfter(h, now); ok {
		t.Error("garbage should be ignored")
	}
}
