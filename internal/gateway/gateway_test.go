package gateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/gateway"
	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/metrics"
	"github.com/GitDaksh/hardcap/internal/mock"
	"github.com/GitDaksh/hardcap/internal/pricing"
	"github.com/GitDaksh/hardcap/internal/ratelimit"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

const chatPath = "/v1/chat/completions"

// Two providers and three models. Prices are per million tokens: "m" costs
// 10 micros per input token and 30 per output token.
const configYAML = `
routing: {attempts_per_route: 2, breaker_failures: 3, breaker_cooldown: 1m}
providers:
  primary: {base_url: "%s"}
  backup: {base_url: "%s", max_tokens_field: max_completion_tokens}
models:
  m:
    default_max_tokens: 10
    max_output_tokens: 100
    prompt_overhead_tokens: 0
    routes:
      - {provider: primary, model: mock-1, input_per_million: 10, output_per_million: 30}
  fallback:
    default_max_tokens: 10
    prompt_overhead_tokens: 0
    routes:
      - {provider: primary, model: mock-1, input_per_million: 10, output_per_million: 30}
      - {provider: backup, model: mock-1, input_per_million: 5, output_per_million: 20}
`

var mPrice = pricing.Price{InputPerMillion: 10_000_000, OutputPerMillion: 30_000_000}

type env struct {
	t       *testing.T
	url     string
	ledger  *ledger.Ledger
	pool    *pgxpool.Pool
	reg     *metrics.Registry
	results chan gateway.Result
}

type options struct {
	backupURL string
	limiter   *ratelimit.Limiter
	timeout   time.Duration
	noResults bool
}

// start runs a gateway in front of primaryURL (and optionally a backup).
func start(t *testing.T, primaryURL string, opts options) *env {
	t.Helper()
	if opts.backupURL == "" {
		opts.backupURL = "http://127.0.0.1:1/v1" // nothing listens there
	}
	models, _, err := config.Parse([]byte(fmt.Sprintf(configYAML, primaryURL, opts.backupURL)), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	pool := testenv.Postgres(t)
	e := &env{t: t, ledger: ledger.New(pool), pool: pool, reg: metrics.NewRegistry(), results: make(chan gateway.Result, 64)}
	cfg := gateway.Config{
		Models: models, Ledger: e.ledger, Limiter: opts.limiter, Metrics: e.reg,
		RequestTimeout: opts.timeout,
	}
	if !opts.noResults {
		cfg.OnComplete = func(r gateway.Result) { e.results <- r }
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e
}

// key creates a project with the given cap and mode, and returns its id and
// an API key for it.
func (e *env) key(cap pricing.Micros, mode ledger.Mode, opts ...ledger.KeyOptions) (int64, string) {
	e.t.Helper()
	ctx := context.Background()
	p, err := e.ledger.CreateProject(ctx, fmt.Sprintf("p%d", time.Now().UnixNano()), cap, mode)
	if err != nil {
		e.t.Fatal(err)
	}
	ko := ledger.KeyOptions{Name: "k"}
	if len(opts) > 0 {
		ko = opts[0]
	}
	_, secret, err := e.ledger.CreateKey(ctx, p.ID, ko)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID, secret
}

func (e *env) post(ctx context.Context, secret, body string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+chatPath, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) result() gateway.Result {
	e.t.Helper()
	select {
	case r := <-e.results:
		return r
	case <-time.After(5 * time.Second):
		e.t.Fatal("timed out waiting for the gateway to finish the request")
		return gateway.Result{}
	}
}

func (e *env) audit(projectID int64) ledger.Audit {
	e.t.Helper()
	a, err := e.ledger.Audit(context.Background(), projectID)
	if err != nil {
		e.t.Fatal(err)
	}
	if !a.Balanced() {
		e.t.Errorf("books don't balance: %+v", a)
	}
	return a
}

func startMock(t *testing.T, cfg mock.Config) string {
	t.Helper()
	srv := httptest.NewServer(mock.NewHandler(cfg))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func startUpstream(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func chat(model string, extra string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]` + extra + `}`
}

// readEvents reads a whole SSE stream and returns each event's data.
func readEvents(t *testing.T, r io.Reader) []string {
	t.Helper()
	var events []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			events = append(events, data)
		}
	}
	return events
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.Error.Code
}

func TestStreamIsHeldThenSettledAtTheRealCost(t *testing.T) {
	e := start(t, startMock(t, mock.Config{}), options{})
	projectID, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	body := chat("m", `,"max_tokens":5,"stream":true`)
	resp := e.post(context.Background(), secret, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	events := readEvents(t, resp.Body)
	// Role chunk + 5 tokens + finish chunk + [DONE]. The usage chunk the
	// gateway asked for is hidden: this client didn't ask for it.
	if len(events) != 8 || events[7] != "[DONE]" {
		t.Fatalf("got %d events:\n%s", len(events), strings.Join(events, "\n"))
	}

	res := e.result()
	wantHeld := mPrice.Cost(int64(len(body)), 5) // the whole body as the prompt bound
	wantCost := mPrice.Cost(4, 5)                // what the mock really reports
	if res.Held != wantHeld || res.Cost != wantCost || res.Outcome != gateway.OutcomeOK {
		t.Fatalf("held %s cost %s outcome %q; want held %s cost %s ok", res.Held, res.Cost, res.Outcome, wantHeld, wantCost)
	}
	if !strings.Contains(resp.Header.Get("Server-Timing"), "admission;dur=") {
		t.Errorf("Server-Timing = %q", resp.Header.Get("Server-Timing"))
	}
	if got := resp.Header.Get("X-Hardcap-Held-Usd"); got != fmt.Sprintf("%.6f", wantHeld.USD()) {
		t.Errorf("X-Hardcap-Held-Usd = %q", got)
	}
	if a := e.audit(projectID); a.Project.Spent != wantCost || a.Project.Reserved != 0 {
		t.Errorf("ledger: %+v", a)
	}
}

func TestWholeReplyReportsItsCost(t *testing.T) {
	e := start(t, startMock(t, mock.Config{}), options{})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	resp := e.post(context.Background(), secret, chat("m", `,"max_tokens":5`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Hardcap-Cost-Usd"); got != fmt.Sprintf("%.6f", mPrice.Cost(4, 5).USD()) {
		t.Errorf("X-Hardcap-Cost-Usd = %q", got)
	}
	if res := e.result(); res.Usage == nil || res.Usage.CompletionTokens != 5 || res.Provider != "primary" {
		t.Errorf("result = %+v", res)
	}
}

func TestBudgetExhaustedIs402AndNothingIsSent(t *testing.T) {
	var calls atomic.Int64
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	e := start(t, upstream, options{})
	_, secret := e.key(100, ledger.ModeHardcap) // $0.0001: far too little

	resp := e.post(context.Background(), secret, chat("m", ""))
	if resp.StatusCode != http.StatusPaymentRequired || errorCode(t, resp) != "budget_exceeded" {
		t.Fatalf("status = %d, want 402 budget_exceeded", resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Error("the provider was called for a request with no budget")
	}
}

func TestKeysAreChecked(t *testing.T) {
	e := start(t, startMock(t, mock.Config{}), options{})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	for name, key := range map[string]string{"none": "", "made up": "hc_" + strings.Repeat("x", 32), "wrong shape": "sk-123"} {
		if resp := e.post(context.Background(), key, chat("m", "")); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
	if resp := e.post(context.Background(), secret, chat("nope", "")); resp.StatusCode != http.StatusNotFound || errorCode(t, resp) != "model_not_found" {
		t.Errorf("unknown model: status = %d, want 404", resp.StatusCode)
	}
}

func TestRejectsBadRequests(t *testing.T) {
	e := start(t, startMock(t, mock.Config{}), options{noResults: true})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)
	huge := chat("m", `,"x":"`+strings.Repeat("a", 1<<20)+`"`)

	cases := []struct {
		name, body string
		want       int
	}{
		{"not JSON", `{nope`, 400},
		{"not an object", `[1,2]`, 400},
		{"trailing junk", chat("m", "") + ` extra`, 400},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, 400},
		{"no messages", `{"model":"m"}`, 400},
		{"stream is not a bool", chat("m", `,"stream":"yes"`), 400},
		{"n of 2", chat("m", `,"n":2`), 400},
		{"zero max_tokens", chat("m", `,"max_tokens":0`), 400},
		{"fractional max_tokens", chat("m", `,"max_tokens":2.5`), 400},
		{"max_tokens above the model's limit", chat("m", `,"max_tokens":101`), 400},
		{"an image", `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`, 400},
		{"over 1 MiB", huge, 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if resp := e.post(context.Background(), secret, c.body); resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
	// Text parts and tool-call messages with no content are fine.
	ok := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":null,"tool_calls":[]}]}`
	if resp := e.post(context.Background(), secret, ok); resp.StatusCode != 200 {
		t.Errorf("text parts: status = %d", resp.StatusCode)
	}
}

func TestEveryRequestGetsAnOutputLimit(t *testing.T) {
	bodies := make(chan map[string]any, 4)
	record := func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies <- body
		http.Error(w, `{"error":{"message":"down"}}`, http.StatusServiceUnavailable)
	}
	e := start(t, startUpstream(t, record), options{backupURL: startUpstream(t, record), noResults: true})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	// No max_tokens at all: the model's default (10) is filled in. The
	// primary reads max_tokens; the backup reads max_completion_tokens.
	// Every provider fails, so we see all 4 tries: 2 per route.
	e.post(context.Background(), secret, chat("fallback", ""))
	first, second, third, fourth := <-bodies, <-bodies, <-bodies, <-bodies
	for _, b := range []map[string]any{first, second} {
		if b["max_tokens"] != float64(10) || b["max_completion_tokens"] != nil || b["model"] != "mock-1" {
			t.Errorf("primary got %v", b)
		}
	}
	for _, b := range []map[string]any{third, fourth} {
		if b["max_completion_tokens"] != float64(10) || b["max_tokens"] != nil {
			t.Errorf("backup got %v", b)
		}
	}
}

func TestForwardsTheRequestFaithfully(t *testing.T) {
	type seen struct{ auth, body string }
	got := make(chan seen, 1)
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Get("Authorization"), string(body)}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	e := start(t, upstream, options{noResults: true})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	e.post(context.Background(), secret, chat("m", `,"max_tokens":7,"temperature":0.7,"seed":1000000,"extra":{"keep":"me <&>"}`))
	s := <-got
	if s.auth != "" {
		t.Errorf("Authorization = %q: the client's key must never reach the provider", s.auth)
	}
	for _, want := range []string{`"model":"mock-1"`, `"max_tokens":7`, `"temperature":0.7`, `"seed":1000000`, `"extra":{"keep":"me <&>"}`} {
		if !strings.Contains(s.body, want) {
			t.Errorf("provider got %s, missing %s", s.body, want)
		}
	}
}

func TestStreamIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	e := start(t, upstream, options{noResults: true})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	resp := e.post(context.Background(), secret, chat("m", `,"stream":true`))
	first := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, "first") {
			t.Fatalf("first line = %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event never arrived while the provider was still generating: the gateway is buffering")
	}
}

func TestClientHangUpCancelsTheProviderAndChargesTheHold(t *testing.T) {
	providerCanceled := make(chan struct{})
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(providerCanceled)
	})
	e := start(t, upstream, options{})
	projectID, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	resp := e.post(ctx, secret, chat("m", `,"stream":true`))
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	hangUp()

	select {
	case <-providerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider call kept running after the client left")
	}
	res := e.result()
	if !res.ClientGone || res.Outcome != gateway.OutcomeClientGone || res.Cost != res.Held {
		t.Fatalf("result = %+v; a reply cut short has unknown cost, so the full hold is charged", res)
	}
	if a := e.audit(projectID); a.Project.Spent != res.Held {
		t.Errorf("ledger: %+v", a)
	}
}

func TestProviderCrashMidStream(t *testing.T) {
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection, like a crashed provider
	})
	e := start(t, upstream, options{})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	events := readEvents(t, e.post(context.Background(), secret, chat("m", `,"stream":true`)).Body)
	if len(events) != 2 || !strings.Contains(events[1], "upstream_error") {
		t.Fatalf("events = %q, want the first chunk, then an upstream_error event", events)
	}
	if res := e.result(); res.Outcome != gateway.OutcomeUpstreamFailed || res.Cost != res.Held {
		t.Errorf("result = %+v", res)
	}
}

func TestProviderErrorsPassThroughAndCostNothing(t *testing.T) {
	e := start(t, startMock(t, mock.Config{FailRate: 1}), options{})
	projectID, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	resp := e.post(context.Background(), secret, chat("m", ""))
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || !strings.Contains(string(body), "simulated provider failure") {
		t.Fatalf("got %d %s, want the provider's own 500", resp.StatusCode, body)
	}
	res := e.result()
	if res.Attempts != 2 || res.Cost != 0 || res.Outcome != gateway.OutcomeProviderError {
		t.Errorf("result = %+v; want 2 attempts (a retry), no charge", res)
	}
	if a := e.audit(projectID); a.Project.Spent != 0 || a.Project.Reserved != 0 || a.ReceiptCount != 1 {
		t.Errorf("ledger: %+v", a)
	}
}

func TestFallsBackToTheNextProvider(t *testing.T) {
	down := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30") // too long to wait: fall back now
		http.Error(w, `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests)
	})
	e := start(t, down, options{backupURL: startMock(t, mock.Config{})})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	body := chat("fallback", `,"max_tokens":5`)
	start := time.Now()
	if resp := e.post(context.Background(), secret, body); resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 from the backup", resp.StatusCode)
	}
	if time.Since(start) > time.Second {
		t.Error("a 30s Retry-After should mean 'fall back now', not 'wait'")
	}
	res := e.result()
	backup := pricing.Price{InputPerMillion: 5_000_000, OutputPerMillion: 20_000_000}
	if res.Provider != "backup" || res.Attempts != 2 {
		t.Fatalf("provider %q after %d attempts", res.Provider, res.Attempts)
	}
	// Held at the most expensive route, charged at the one that answered.
	if res.Held != mPrice.Cost(int64(len(body)), 5) || res.Cost != backup.Cost(4, 5) {
		t.Errorf("held %s cost %s", res.Held, res.Cost)
	}
}

func TestBreakerSkipsADeadProvider(t *testing.T) {
	e := start(t, "http://127.0.0.1:1/v1", options{backupURL: startMock(t, mock.Config{})})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	// Each request tries the dead primary twice (connection refused, which
	// is safe to retry), then the backup. After 3 failures the primary's
	// breaker opens and later requests go straight to the backup.
	var attempts []int
	for range 4 {
		if resp := e.post(context.Background(), secret, chat("fallback", "")); resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		attempts = append(attempts, e.result().Attempts)
	}
	if fmt.Sprint(attempts) != "[3 2 1 1]" {
		t.Errorf("attempts per request = %v, want [3 2 1 1]", attempts)
	}
	if !strings.Contains(scrape(t, e.reg), `hardcap_breaker_state{provider="primary"} 2`) {
		t.Error("the primary's breaker should be reported open")
	}
}

func TestNoRetryAfterAnAmbiguousFailure(t *testing.T) {
	var backupCalls atomic.Int64
	hang := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Go's server only notices a client hang-up after the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	backup := startUpstream(t, func(w http.ResponseWriter, r *http.Request) { backupCalls.Add(1) })
	e := start(t, hang, options{backupURL: backup, timeout: 300 * time.Millisecond})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	resp := e.post(context.Background(), secret, chat("fallback", ""))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	res := e.result()
	if backupCalls.Load() != 0 || res.Attempts != 1 {
		t.Error("after a timeout the provider may be generating: retrying could pay twice")
	}
	if res.Cost != res.Held || res.Outcome != gateway.OutcomeUpstreamFailed {
		t.Errorf("result = %+v; unknown cost means the full hold", res)
	}
}

func TestGroqStyleUsage(t *testing.T) {
	// Groq reports usage under x_groq, on the final chunk that also has choices.
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"x_groq\":{\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	e := start(t, upstream, options{})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)

	if events := readEvents(t, e.post(context.Background(), secret, chat("m", `,"stream":true`)).Body); len(events) != 3 {
		t.Fatalf("events = %q", events)
	}
	if res := e.result(); res.Usage == nil || res.Usage.PromptTokens != 12 || res.Cost != mPrice.Cost(12, 3) {
		t.Errorf("result = %+v", res)
	}
}

func TestRateLimits(t *testing.T) {
	client, prefix := testenv.Redis(t)
	e := start(t, startMock(t, mock.Config{}), options{limiter: ratelimit.New(client, prefix), noResults: true})
	two := 2
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap, ledger.KeyOptions{Name: "slow", RPM: &two})

	for i := range 2 {
		resp := e.post(context.Background(), secret, chat("m", ""))
		if resp.StatusCode != 200 || resp.Header.Get("X-Ratelimit-Remaining-Requests") != fmt.Sprint(1-i) {
			t.Fatalf("request %d: %d, remaining %q", i+1, resp.StatusCode, resp.Header.Get("X-Ratelimit-Remaining-Requests"))
		}
	}
	resp := e.post(context.Background(), secret, chat("m", ""))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("3rd request: %d, Retry-After %q; want 429 and 30s", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestRateLimiterOutageFailsOpen(t *testing.T) {
	dead := ratelimit.New(redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1}), "x:")
	e := start(t, startMock(t, mock.Config{}), options{limiter: dead, noResults: true})
	limit := 100 // with no limit set, Redis wouldn't even be asked
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap, ledger.KeyOptions{Name: "k", RPM: &limit})

	if resp := e.post(context.Background(), secret, chat("m", "")); resp.StatusCode != 200 {
		t.Fatalf("status = %d: a rate limiter outage must not block traffic", resp.StatusCode)
	}
	if !strings.Contains(scrape(t, e.reg), "hardcap_ratelimit_errors_total 1") {
		t.Error("the outage should be counted")
	}
}

func TestBudgetStoreOutageFailsClosed(t *testing.T) {
	var calls atomic.Int64
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	e := start(t, upstream, options{noResults: true})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)
	e.pool.Close() // Postgres "goes down"

	resp := e.post(context.Background(), secret, chat("m", ""))
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, resp) != "budget_store_unavailable" {
		t.Fatalf("status = %d, want 503 budget_store_unavailable", resp.StatusCode)
	}
	if calls.Load() != 0 {
		t.Error("with the budget store down, nothing may be sent to the provider")
	}
}

func TestModelsList(t *testing.T) {
	e := start(t, startMock(t, mock.Config{}), options{noResults: true})
	_, secret := e.key(pricing.PerDollar, ledger.ModeHardcap)
	req, _ := http.NewRequest(http.MethodGet, e.url+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 2 || list.Data[0].ID != "fallback" || list.Data[1].ID != "m" {
		t.Errorf("models = %+v", list.Data)
	}
}

// End to end: 300 clients streaming through the gateway at once, in a loop,
// against a 10-cent cap. Naive mode blows through the cap; hardcap mode
// fills it to within one hold and stops.
func TestStampedeThroughTheGateway(t *testing.T) {
	for _, mode := range []ledger.Mode{ledger.ModeNaive, ledger.ModeHardcap} {
		t.Run(string(mode), func(t *testing.T) {
			e := start(t, startMock(t, mock.Config{TokenDelay: time.Millisecond}), options{noResults: true})
			const cap = 100_000 // $0.10
			projectID, secret := e.key(cap, mode)

			body := chat("m", `,"max_tokens":5,"stream":true`)
			client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 300}}
			var ok, refused atomic.Int64
			var wg sync.WaitGroup
			for range 300 {
				wg.Go(func() {
					for misses := 0; misses < 5; {
						req, _ := http.NewRequest(http.MethodPost, e.url+chatPath, strings.NewReader(body))
						req.Header.Set("Authorization", "Bearer "+secret)
						resp, err := client.Do(req)
						if err != nil {
							t.Error(err)
							return
						}
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						switch resp.StatusCode {
						case 200:
							ok.Add(1)
						case 402:
							refused.Add(1)
							misses++
						default:
							t.Errorf("status %d", resp.StatusCode)
							return
						}
					}
				})
			}
			wg.Wait()

			a := e.audit(projectID)
			t.Logf("%s: %d served, %d refused, spent %s of a %s cap", mode, ok.Load(), refused.Load(), a.Project.Spent, a.Project.Cap)
			if a.Project.Reserved != 0 {
				t.Errorf("holds left open: %+v", a)
			}
			hold := mPrice.Cost(int64(len(body)), 5)
			// Naive mode usually overspends here, but how much depends on
			// timing, so only hardcap mode is held to an exact promise.
			if mode == ledger.ModeHardcap {
				if a.Project.Spent > cap {
					t.Fatalf("CAP CROSSED: spent %s of %s", a.Project.Spent, a.Project.Cap)
				}
				if a.Project.Spent+hold < cap {
					t.Errorf("spent %s: should fill to within one hold (%s) of the cap", a.Project.Spent, hold)
				}
			}
		})
	}
}

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}
