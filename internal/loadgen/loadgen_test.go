package loadgen

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCountsOutcomes(t *testing.T) {
	// A fake gateway: 10 successes, then budget refusals forever.
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hc_test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if served.Add(1) > 10 {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		w.Header().Set("Server-Timing", "auth;dur=0.1, admission;dur=1.5")
		fmt.Fprint(w, "data: {}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	var stats Stats
	Run(context.Background(), Config{URL: srv.URL, APIKey: "hc_test", Model: "m", Workers: 4, StopAfterRefusals: 3, Duration: 10 * time.Second}, &stats)

	s := stats.Snapshot()
	if s.Running || s.OK != 10 || s.Refused != 12 || s.Failed != 0 || s.Sent != 22 {
		t.Fatalf("snapshot = %+v; want 10 ok and 12 refused (4 workers x 3 in a row)", s)
	}
	if s.AdmitP50ms != 1.5 {
		t.Errorf("admission p50 = %v, want 1.5ms from Server-Timing", s.AdmitP50ms)
	}
}

// When the run's time is up, workers stop sending, but a reply that's
// halfway through gets to finish instead of being cut off.
func TestRunLetsRequestsInFlightFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var stats Stats
	Run(context.Background(), Config{URL: srv.URL, Model: "m", Workers: 3, Duration: 100 * time.Millisecond}, &stats)

	if s := stats.Snapshot(); s.Sent != 3 || s.OK != 3 || s.Failed != 0 {
		t.Fatalf("snapshot = %+v; want each worker's one request to finish", s)
	}
}

func TestPercentiles(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	if pct(d, 50) != 50 || pct(d, 99) != 99 || pct(d, 100) != 100 || pct(nil, 50) != 0 {
		t.Errorf("p50=%v p99=%v p100=%v", pct(d, 50), pct(d, 99), pct(d, 100))
	}
}

func TestAdmissionHeader(t *testing.T) {
	cases := map[string]float64{
		"admission;dur=2.5":                2.5,
		"auth;dur=0.1, admission;dur=0.75": 0.75,
		`admission;desc="x";dur=3`:         3,
		"auth;dur=1":                       -1,
		"":                                 -1,
	}
	for h, want := range cases {
		d, ok := admission(h)
		if want < 0 {
			if ok {
				t.Errorf("%q: got %v, want nothing", h, d)
			}
			continue
		}
		if !ok || d != time.Duration(want*float64(time.Millisecond)) {
			t.Errorf("%q: got %v %v, want %vms", h, d, ok, want)
		}
	}
}
