package metrics_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GitDaksh/hardcap/internal/metrics"
)

func scrape(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestExpositionFormat(t *testing.T) {
	r := metrics.NewRegistry()
	reqs := r.Counter("app_requests_total", "Requests served.", "code")
	reqs.With("200").Add(3)
	reqs.With("402").Inc()
	inflight := r.Gauge("app_inflight", "Requests in flight.")
	inflight.With().Set(7)
	lat := r.Histogram("app_seconds", "Latency.", []float64{0.1, 1}, "route")
	lat.With("/x").Observe(0.05)
	lat.With("/x").Observe(0.5)
	lat.With("/x").Observe(3)
	r.GaugeFunc("app_up", `Is it "up"? Yes\no`, func() float64 { return 1 })

	want := `# HELP app_requests_total Requests served.
# TYPE app_requests_total counter
app_requests_total{code="200"} 3
app_requests_total{code="402"} 1
# HELP app_inflight Requests in flight.
# TYPE app_inflight gauge
app_inflight 7
# HELP app_seconds Latency.
# TYPE app_seconds histogram
app_seconds_bucket{route="/x",le="0.1"} 1
app_seconds_bucket{route="/x",le="1"} 2
app_seconds_bucket{route="/x",le="+Inf"} 3
app_seconds_sum{route="/x"} 3.55
app_seconds_count{route="/x"} 3
# HELP app_up Is it "up"? Yes\\no
# TYPE app_up gauge
app_up 1
`
	if got := scrape(t, r); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestLabelValuesAreEscaped(t *testing.T) {
	r := metrics.NewRegistry()
	r.Counter("c", "h", "v").With("a\"b\\c\nd").Inc()
	if got := scrape(t, r); !strings.Contains(got, `c{v="a\"b\\c\nd"} 1`) {
		t.Errorf("got %s", got)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	r := metrics.NewRegistry()
	c := r.Counter("c", "h")
	h := r.Histogram("h", "h", metrics.DefaultLatencyBuckets)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 1000 {
				c.With().Inc()
				h.With().Observe(0.01)
			}
		})
	}
	wg.Wait()
	if c.With().Value() != 50_000 || h.With().Count() != 50_000 {
		t.Errorf("counter = %v, histogram count = %d; want 50000 each", c.With().Value(), h.With().Count())
	}
}

func TestDuplicateNamesPanic(t *testing.T) {
	r := metrics.NewRegistry()
	r.Counter("x", "h")
	defer func() {
		if recover() == nil {
			t.Error("registering a name twice should panic")
		}
	}()
	r.Gauge("x", "h")
}
