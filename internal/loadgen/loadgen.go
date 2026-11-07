// Package loadgen fires concurrent chat requests at a Hardcap gateway and
// records what happened. The stampede CLI and the dashboard's Stampede
// button both use it.
package loadgen

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes one load run.
type Config struct {
	URL       string // the gateway, e.g. http://localhost:8080
	APIKey    string
	Model     string
	Workers   int           // clients sending requests at the same time
	Duration  time.Duration // stop after this long (0: no time limit)
	MaxTokens int           // 0: the model's default
	Stream    bool
	// StopAfterRefusals makes each worker stop after this many budget
	// refusals (HTTP 402) in a row. 0 means keep going until Duration.
	StopAfterRefusals int
}

// Stats is collected while a run is going. It's safe to read a Snapshot
// at any time, from any goroutine.
type Stats struct {
	sent, ok, refused, limited, failed atomic.Int64
	running                            atomic.Bool

	mu        sync.Mutex
	total     []time.Duration // per successful request
	firstByte []time.Duration
	admission []time.Duration // Hardcap's own overhead, from Server-Timing
	lastError string
}

// maxSamples bounds memory on very long runs.
const maxSamples = 1_000_000

// Snapshot is a point-in-time summary of a run.
type Snapshot struct {
	Running     bool    `json:"running"`
	Sent        int64   `json:"sent"`
	OK          int64   `json:"ok"`
	Refused     int64   `json:"refused"`      // 402: over budget
	RateLimited int64   `json:"rate_limited"` // 429
	Failed      int64   `json:"failed"`       // anything else
	LastError   string  `json:"last_error,omitempty"`
	P50ms       float64 `json:"p50_ms"`
	P95ms       float64 `json:"p95_ms"`
	P99ms       float64 `json:"p99_ms"`
	TTFBp50ms   float64 `json:"ttfb_p50_ms"`
	TTFBp99ms   float64 `json:"ttfb_p99_ms"`
	AdmitP50ms  float64 `json:"admission_p50_ms"`
	AdmitP99ms  float64 `json:"admission_p99_ms"`
}

// Snapshot summarizes the run so far.
func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	total, ttfb, admit := slices.Clone(s.total), slices.Clone(s.firstByte), slices.Clone(s.admission)
	lastErr := s.lastError
	s.mu.Unlock()
	slices.Sort(total)
	slices.Sort(ttfb)
	slices.Sort(admit)
	return Snapshot{
		Running: s.running.Load(), Sent: s.sent.Load(), OK: s.ok.Load(), Refused: s.refused.Load(),
		RateLimited: s.limited.Load(), Failed: s.failed.Load(), LastError: lastErr,
		P50ms: pct(total, 50), P95ms: pct(total, 95), P99ms: pct(total, 99),
		TTFBp50ms: pct(ttfb, 50), TTFBp99ms: pct(ttfb, 99),
		AdmitP50ms: pct(admit, 50), AdmitP99ms: pct(admit, 99),
	}
}

// pct returns the p-th percentile of sorted, in milliseconds.
func pct(sorted []time.Duration, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := (len(sorted)*p+99)/100 - 1
	return float64(sorted[max(i, 0)].Microseconds()) / 1000
}

// Run sends requests until ctx ends, Duration passes, or every worker has
// hit StopAfterRefusals.
func Run(ctx context.Context, cfg Config, stats *Stats) {
	stats.running.Store(true)
	defer stats.running.Store(false)
	if cfg.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Duration)
		defer cancel()
	}

	payload := map[string]any{
		"model":    cfg.Model,
		"messages": []map[string]string{{"role": "user", "content": "Say something short."}},
		"stream":   cfg.Stream,
	}
	if cfg.MaxTokens > 0 {
		payload["max_tokens"] = cfg.MaxTokens
	}
	body, _ := json.Marshal(payload)

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = cfg.Workers
	transport.MaxIdleConns = cfg.Workers
	client := &http.Client{Transport: transport}
	defer transport.CloseIdleConnections()

	url := strings.TrimRight(cfg.URL, "/") + "/v1/chat/completions"
	var wg sync.WaitGroup
	for range max(cfg.Workers, 1) {
		wg.Go(func() {
			refusedInARow := 0
			for ctx.Err() == nil {
				switch stats.one(ctx, client, url, cfg.APIKey, body) {
				case http.StatusPaymentRequired:
					refusedInARow++
					if cfg.StopAfterRefusals > 0 && refusedInARow >= cfg.StopAfterRefusals {
						return
					}
					sleep(ctx, 5*time.Millisecond) // a refused client backs off a little
				case http.StatusTooManyRequests, 0:
					refusedInARow = 0
					sleep(ctx, 50*time.Millisecond)
				default:
					refusedInARow = 0
				}
			}
		})
	}
	wg.Wait()
}

// one sends a single request and returns its status (0 for a network error).
func (s *Stats) one(ctx context.Context, client *http.Client, url, key string, body []byte) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	start := time.Now()
	s.sent.Add(1)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			s.failed.Add(1)
			s.noteError(err.Error())
		} else {
			s.sent.Add(-1) // cut off by the end of the run: don't count it
		}
		return 0
	}
	defer resp.Body.Close()

	// Time to first byte: when the first event (or the whole reply) begins.
	br := bufio.NewReader(resp.Body)
	_, _ = br.Peek(1)
	ttfb := time.Since(start)
	_, err = io.Copy(io.Discard, br)
	total := time.Since(start)

	switch {
	case resp.StatusCode == http.StatusOK && err == nil:
		s.ok.Add(1)
		s.mu.Lock()
		if len(s.total) < maxSamples {
			s.total = append(s.total, total)
			s.firstByte = append(s.firstByte, ttfb)
			if d, ok := admission(resp.Header.Get("Server-Timing")); ok {
				s.admission = append(s.admission, d)
			}
		}
		s.mu.Unlock()
	case resp.StatusCode == http.StatusPaymentRequired:
		s.refused.Add(1)
	case resp.StatusCode == http.StatusTooManyRequests:
		s.limited.Add(1)
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		s.sent.Add(-1)
	default:
		s.failed.Add(1)
		s.noteError("HTTP " + strconv.Itoa(resp.StatusCode))
	}
	return resp.StatusCode
}

func (s *Stats) noteError(msg string) {
	s.mu.Lock()
	s.lastError = msg
	s.mu.Unlock()
}

// admission reads "admission;dur=1.234" from a Server-Timing header.
func admission(header string) (time.Duration, bool) {
	for part := range strings.SplitSeq(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if name != "admission" {
			continue
		}
		for p := range strings.SplitSeq(params, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(p), "dur="); ok {
				ms, err := strconv.ParseFloat(v, 64)
				if err == nil {
					return time.Duration(ms * float64(time.Millisecond)), true
				}
			}
		}
	}
	return 0, false
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
