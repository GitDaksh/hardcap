package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GitDaksh/hardcap/internal/ratelimit"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

func TestRequestsPerMinute(t *testing.T) {
	client, prefix := testenv.Redis(t)
	l := ratelimit.New(client, prefix)
	ctx := context.Background()

	for i := range 3 {
		d, err := l.Allow(ctx, 1, 3, 0, 10)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d: %+v, %v", i+1, d, err)
		}
		if d.RemainingRequests != int64(2-i) {
			t.Errorf("request %d: remaining = %d, want %d", i+1, d.RemainingRequests, 2-i)
		}
	}
	d, err := l.Allow(ctx, 1, 3, 0, 10)
	if err != nil || d.Allowed || d.Reason != "rpm" {
		t.Fatalf("4th request: %+v, %v; want refused for rpm", d, err)
	}
	// 3 per minute refills one every 20s.
	if d.RetryAfter < 19*time.Second || d.RetryAfter > 20*time.Second {
		t.Errorf("RetryAfter = %v, want about 20s", d.RetryAfter)
	}

	// Another key has its own buckets.
	if d, _ := l.Allow(ctx, 2, 3, 0, 10); !d.Allowed {
		t.Error("key 2 should not be limited by key 1's traffic")
	}
}

func TestTokensPerMinute(t *testing.T) {
	client, prefix := testenv.Redis(t)
	l := ratelimit.New(client, prefix)
	ctx := context.Background()

	if d, _ := l.Allow(ctx, 1, 100, 1000, 700); !d.Allowed || d.RemainingTokens != 300 {
		t.Fatalf("first: %+v", d)
	}
	d, _ := l.Allow(ctx, 1, 100, 1000, 500)
	if d.Allowed || d.Reason != "tpm" {
		t.Fatalf("second: %+v, want refused for tpm", d)
	}
	// The refused request must not have used up a request slot.
	if d, _ := l.Allow(ctx, 1, 100, 1000, 300); !d.Allowed || d.RemainingRequests != 98 {
		t.Fatalf("third: %+v, want allowed with 98 requests left", d)
	}
}

func TestTooLargeNeverFits(t *testing.T) {
	client, prefix := testenv.Redis(t)
	l := ratelimit.New(client, prefix)
	d, err := l.Allow(context.Background(), 1, 0, 1000, 5000)
	if err != nil || d.Allowed || d.Reason != "too_large" {
		t.Fatalf("%+v, %v; want too_large", d, err)
	}
}

func TestNoLimitsSkipsRedis(t *testing.T) {
	// A client pointing nowhere: with no limits set, Redis is never called.
	l := ratelimit.New(redis.NewClient(&redis.Options{Addr: "localhost:1"}), "x:")
	if d, err := l.Allow(context.Background(), 1, 0, 0, 1_000_000); err != nil || !d.Allowed {
		t.Fatalf("%+v, %v", d, err)
	}
}

func TestRedisDownIsAnErrorNotARefusal(t *testing.T) {
	l := ratelimit.New(redis.NewClient(&redis.Options{Addr: "localhost:1", MaxRetries: -1}), "x:")
	if _, err := l.Allow(context.Background(), 1, 10, 0, 1); err == nil {
		t.Fatal("want an error, so the gateway can fail open")
	}
}

// 50 goroutines race for a 20-per-minute limit: exactly 20 win, because the
// script checks and charges atomically.
func TestConcurrentRequestsCantOvershoot(t *testing.T) {
	client, prefix := testenv.Redis(t)
	l := ratelimit.New(client, prefix)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			d, err := l.Allow(context.Background(), 7, 20, 0, 1)
			if err != nil {
				t.Error(err)
			}
			if d.Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 20 {
		t.Fatalf("allowed %d, want exactly 20", got)
	}
}

func TestBucketsRefill(t *testing.T) {
	client, prefix := testenv.Redis(t)
	l := ratelimit.New(client, prefix)
	ctx := context.Background()
	// 600 per minute = one every 100ms.
	for range 600 {
		if d, _ := l.Allow(ctx, 1, 600, 0, 1); !d.Allowed {
			t.Fatal("the first 600 should all fit")
		}
	}
	if d, _ := l.Allow(ctx, 1, 600, 0, 1); d.Allowed {
		t.Fatal("601st should be refused")
	}
	time.Sleep(250 * time.Millisecond)
	if d, _ := l.Allow(ctx, 1, 600, 0, 1); !d.Allowed {
		t.Fatal("after 250ms, about 2 slots should have refilled")
	}
}
