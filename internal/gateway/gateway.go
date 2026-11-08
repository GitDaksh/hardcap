// Package gateway is the heart of Hardcap: an OpenAI-compatible proxy that
// checks every request against its budget before any money is spent.
//
// For each request it:
//
//  1. authenticates the API key,
//  2. reads the request and gives it a max_tokens (no unbounded replies),
//  3. checks the key's rate limits in Redis,
//  4. holds the worst-case cost in Postgres, or refuses with HTTP 402,
//  5. sends it to the first healthy provider, retrying or falling back only
//     when the failed try provably generated nothing,
//  6. streams the reply back, reading the real token usage on the way,
//  7. settles: releases the hold and charges the real cost.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/metrics"
	"github.com/GitDaksh/hardcap/internal/pricing"
	"github.com/GitDaksh/hardcap/internal/ratelimit"
	"github.com/GitDaksh/hardcap/internal/upstream"
)

const (
	maxRequestBytes  = 1 << 20  // 1 MiB is plenty for any chat request
	maxResponseBytes = 16 << 20 // the biggest non-streaming reply held in memory
	maxErrorBytes    = 64 << 10 // the most of a provider's error reply we keep
)

// Config is everything needed to build a Gateway.
type Config struct {
	Models  *config.Config     // providers, models, routes, prices and limits
	Ledger  *ledger.Ledger     // budgets
	Limiter *ratelimit.Limiter // nil turns rate limiting off
	Logger  *slog.Logger       // nil turns logging off
	Metrics *metrics.Registry  // nil keeps the metrics private

	// RequestTimeout is the longest one request may run. Default 5 minutes.
	RequestTimeout time.Duration
	// HoldGrace is how much longer than RequestTimeout a hold lives before
	// the sweeper may charge it. Default 1 minute.
	HoldGrace time.Duration

	// OnComplete, if set, is called once for every finished request.
	OnComplete func(Result)
}

// Usage is the token count an OpenAI-compatible provider reports.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Result describes one finished request.
type Result struct {
	RequestID string
	KeyID     int64
	ProjectID int64
	Model     string // the model the client asked for
	Provider  string // the provider that answered; "" if none did
	Stream    bool
	Status    int    // HTTP status sent to the client; 0 if none was sent
	Outcome   string // see the outcome constants
	Usage     *Usage // nil if the provider never reported usage
	Held      pricing.Micros
	Cost      pricing.Micros
	Attempts  int // calls made to providers
	Events    int // stream events received from the provider

	ClientGone     bool // the client hung up before the reply finished
	UpstreamFailed bool // a provider timed out or broke off mid-reply

	Admission time.Duration // time spent on auth, rate limit and reserve
	Duration  time.Duration

	route          config.Route // the route that answered
	maybeGenerated bool         // a provider may have produced tokens
}

// Outcomes, as recorded in metrics and on receipts.
const (
	OutcomeOK             = "ok"
	OutcomeUsageMissing   = "usage_missing"   // replied, but never said how many tokens: charged the full hold
	OutcomeClientGone     = "client_gone"     // the client hung up
	OutcomeUpstreamFailed = "upstream_failed" // a provider timed out or broke off mid-reply
	OutcomeProviderError  = "provider_error"  // a provider refused the request: nothing generated
	OutcomeUnavailable    = "unavailable"     // no provider could take it: nothing generated

	OutcomeUnauthorized     = "unauthorized"
	OutcomeBadRequest       = "bad_request"
	OutcomeRateLimited      = "rate_limited"
	OutcomeBudgetExceeded   = "budget_exceeded"
	OutcomeStoreUnavailable = "store_unavailable"
)

// Gateway forwards chat requests to AI providers, within budget.
type Gateway struct {
	models         *config.Config
	ledger         *ledger.Ledger
	limiter        *ratelimit.Limiter
	providers      map[string]*provider
	client         *http.Client
	logger         *slog.Logger
	m              *gatewayMetrics
	requestTimeout time.Duration
	holdTTL        time.Duration
	onComplete     func(Result)
}

type provider struct {
	name    string
	cfg     *config.Provider
	chatURL string
	breaker *upstream.Breaker
}

// New checks cfg and builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Models == nil || cfg.Ledger == nil {
		return nil, errors.New("gateway: Models and Ledger are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	reg := cfg.Metrics
	if reg == nil {
		reg = metrics.NewRegistry()
	}
	m := newGatewayMetrics(reg)

	g := &Gateway{
		models:         cfg.Models,
		ledger:         cfg.Ledger,
		limiter:        cfg.Limiter,
		providers:      map[string]*provider{},
		logger:         logger,
		m:              m,
		requestTimeout: cfg.RequestTimeout,
		onComplete:     cfg.OnComplete,
	}
	if g.requestTimeout <= 0 {
		g.requestTimeout = 5 * time.Minute
	}
	grace := cfg.HoldGrace
	if grace <= 0 {
		grace = time.Minute
	}
	g.holdTTL = g.requestTimeout + grace

	routing := cfg.Models.Routing
	for name, p := range cfg.Models.Providers {
		m.breakerState.With(name).Set(float64(upstream.Closed))
		g.providers[name] = &provider{
			name:    name,
			cfg:     p,
			chatURL: p.BaseURL + "/chat/completions",
			breaker: upstream.NewBreaker(routing.BreakerFailures, routing.BreakerCooldown, func(s upstream.State) {
				m.breakerState.With(name).Set(float64(s))
				logger.Warn("circuit breaker changed", "provider", name, "state", s.String())
			}),
		}
	}

	// Go's default client keeps only 2 idle connections per host. A proxy
	// sends everything to a few hosts, so under load it would keep opening
	// new TCP (and TLS) connections: slow, and it can run out of ports.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 1024
	transport.MaxIdleConnsPerHost = 512
	// A non-streaming reply sends its headers only after the whole answer is
	// generated. The request's own deadline (RequestTimeout) still applies.
	transport.ResponseHeaderTimeout = g.requestTimeout
	g.client = &http.Client{Transport: transport} // no overall timeout: streams can run long
	return g, nil
}

// Handler returns the OpenAI-compatible routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chatCompletions)
	mux.HandleFunc("GET /v1/models", g.listModels)
	return mux
}

func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	res := Result{RequestID: newRequestID()}
	w.Header().Set("X-Request-Id", res.RequestID)
	g.m.inFlight.With().Add(1)
	defer func() {
		g.m.inFlight.With().Add(-1)
		res.Duration = time.Since(start)
		g.finish(res)
	}()

	// 1. Who is asking?
	stage := time.Now()
	key, ok := g.authenticate(w, r, &res)
	if !ok {
		return
	}
	timing := []string{serverTiming("auth", time.Since(stage))}

	// 2. What are they asking for?
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			g.fail(w, &res, OutcomeBadRequest, badRequest(http.StatusRequestEntityTooLarge, "request body is larger than 1 MiB"))
		} else {
			g.fail(w, &res, OutcomeBadRequest, badRequest(http.StatusBadRequest, "could not read the request body"))
		}
		return
	}
	req, apiErr := parseChatRequest(body, g.models.Models)
	if apiErr != nil {
		g.fail(w, &res, OutcomeBadRequest, apiErr)
		return
	}
	res.Model, res.Stream = req.model.Name, req.stream

	// 3. Are they going too fast?
	stage = time.Now()
	if !g.checkRateLimit(w, r, key, req, &res) {
		return
	}
	timing = append(timing, serverTiming("ratelimit", time.Since(stage)))

	// 4. Hold the worst case. This runs on a context the client can't
	// cancel: if the client hung up mid-query, we couldn't tell whether the
	// hold was written, and an unknown hold would later be charged in full.
	stage = time.Now()
	amount := req.worstCost()
	reserveCtx, cancelReserve := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	hold, err := g.ledger.Reserve(reserveCtx, key, res.RequestID, req.model.Name, amount, g.holdTTL)
	cancelReserve()
	if err != nil {
		g.refuse(w, &res, err)
		return
	}
	res.Held = amount
	res.Admission = time.Since(start)
	timing = append(timing, serverTiming("reserve", time.Since(stage)), serverTiming("admission", res.Admission))
	w.Header().Set("Server-Timing", strings.Join(timing, ", "))
	w.Header().Set("X-Hardcap-Held-Usd", usdHeader(amount))

	// 5. Whatever happens from here (success, error, hang-up, panic), settle.
	defer g.settle(r.Context(), hold, &res)

	ctx, cancel := context.WithTimeout(r.Context(), g.requestTimeout)
	defer cancel()

	// 6. Find a provider that answers.
	att, err := g.dial(ctx, req, &res)
	if err != nil {
		g.failUpstream(w, r, err, &res)
		return
	}
	defer att.resp.Body.Close()
	res.Provider, res.route = att.provider.name, att.route

	// 7. Relay the reply.
	if req.stream && att.resp.StatusCode == http.StatusOK && isEventStream(att.resp) {
		g.relayStream(w, r, att.resp, req.clientWantsUsage, &res)
	} else {
		g.relayWhole(w, r, att.resp, &res)
	}
	att.report(!res.UpstreamFailed)
}

// settle charges the request's real cost, or the full hold when the cost is
// unknown and tokens may have been generated.
func (g *Gateway) settle(clientCtx context.Context, hold ledger.Hold, res *Result) {
	switch {
	case res.Usage != nil:
		res.Cost = res.route.Price().Cost(int64(res.Usage.PromptTokens), int64(res.Usage.CompletionTokens))
		if res.Cost > hold.Amount {
			// The usage broke the bound the hold was based on (more tokens
			// than bytes). Charge the truth, and make the breach visible.
			g.m.holdExceeded.With().Inc()
			g.logger.Warn("request cost more than its hold", "request_id", res.RequestID,
				"held", hold.Amount.String(), "cost", res.Cost.String())
		}
	case res.maybeGenerated:
		res.Cost = hold.Amount // unknown cost: assume the worst
	default:
		res.Cost = 0 // no provider generated anything
	}
	if res.Outcome == "" {
		res.Outcome = OutcomeOK
		if res.Usage == nil {
			res.Outcome = OutcomeUsageMissing
		}
	}

	settlement := ledger.Settlement{
		Provider:   res.Provider,
		Outcome:    res.Outcome,
		UsageKnown: res.Usage != nil,
		Cost:       res.Cost,
		Duration:   res.Duration,
	}
	if res.Usage != nil {
		settlement.PromptTokens, settlement.CompletionTokens = res.Usage.PromptTokens, res.Usage.CompletionTokens
	}
	// The client may be gone, but the books still need closing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(clientCtx), 10*time.Second)
	defer cancel()
	if err := g.ledger.Settle(ctx, hold, settlement); err != nil {
		g.m.settleErrors.With().Inc()
		g.logger.Error("settle failed; the sweeper will charge the full hold when it expires",
			"request_id", res.RequestID, "error", err)
	}
}

func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, res *Result) (ledger.Key, bool) {
	key, err := g.ledger.Authenticate(r.Context(), bearerToken(r))
	switch {
	case err == nil:
		res.KeyID, res.ProjectID = key.ID, key.ProjectID
		return key, true
	case errors.Is(err, ledger.ErrInvalidKey):
		g.fail(w, res, OutcomeUnauthorized, &apiError{status: http.StatusUnauthorized, typ: "invalid_request_error",
			code: "invalid_api_key", message: "missing or invalid API key: send `Authorization: Bearer hc_...`"})
	default:
		g.logger.Error("can't check API keys", "error", err)
		g.fail(w, res, OutcomeStoreUnavailable, storeUnavailable())
	}
	return ledger.Key{}, false
}

// checkRateLimit applies the key's limits. If Redis is down it lets the
// request through: rate limits protect availability, so an outage of the
// limiter shouldn't become an outage of everything. Budgets, which protect
// money, are never skipped.
func (g *Gateway) checkRateLimit(w http.ResponseWriter, r *http.Request, key ledger.Key, req *chatRequest, res *Result) bool {
	if g.limiter == nil {
		return true
	}
	rpm, tpm := g.models.Limits.DefaultRPM, g.models.Limits.DefaultTPM
	if key.RPM != nil {
		rpm = *key.RPM
	}
	if key.TPM != nil {
		tpm = *key.TPM
	}
	d, err := g.limiter.Allow(r.Context(), key.ID, rpm, tpm, req.worstTokens())
	if err != nil {
		g.m.rateLimitErrors.With().Inc()
		g.logger.Warn("rate limiter unavailable; letting the request through", "error", err)
		return true
	}
	h := w.Header()
	if rpm > 0 {
		h.Set("X-Ratelimit-Limit-Requests", fmt.Sprint(rpm))
		h.Set("X-Ratelimit-Remaining-Requests", fmt.Sprint(d.RemainingRequests))
	}
	if tpm > 0 {
		h.Set("X-Ratelimit-Limit-Tokens", fmt.Sprint(tpm))
		h.Set("X-Ratelimit-Remaining-Tokens", fmt.Sprint(d.RemainingTokens))
	}
	if d.Allowed {
		return true
	}
	if d.Reason == "too_large" {
		g.fail(w, res, OutcomeRateLimited, &apiError{status: http.StatusBadRequest, typ: "invalid_request_error",
			code: "request_too_large", message: fmt.Sprintf(
				"this request could use up to %d tokens, more than this key's limit of %d tokens per minute; lower max_tokens",
				req.worstTokens(), tpm)})
		return false
	}
	h.Set("Retry-After", fmt.Sprint(int((d.RetryAfter+time.Second-1)/time.Second)))
	what := "requests"
	if d.Reason == "tpm" {
		what = "tokens"
	}
	g.fail(w, res, OutcomeRateLimited, &apiError{status: http.StatusTooManyRequests, typ: "rate_limit_error",
		code: "rate_limit_exceeded", message: fmt.Sprintf("rate limit reached for %s per minute; try again in %s",
			what, d.RetryAfter.Round(time.Millisecond))})
	return false
}

// refuse answers a failed Reserve.
func (g *Gateway) refuse(w http.ResponseWriter, res *Result, err error) {
	var be *ledger.BudgetError
	switch {
	case errors.As(err, &be):
		g.m.budgetRejections.With(be.Scope).Inc()
		g.fail(w, res, OutcomeBudgetExceeded, &apiError{status: http.StatusPaymentRequired, typ: "insufficient_quota",
			code: "budget_exceeded", message: be.Error()})
	case errors.Is(err, ledger.ErrInvalidKey):
		g.fail(w, res, OutcomeUnauthorized, &apiError{status: http.StatusUnauthorized, typ: "invalid_request_error",
			code: "invalid_api_key", message: "this API key has been revoked"})
	default:
		g.logger.Error("can't reserve budget", "request_id", res.RequestID, "error", err)
		g.fail(w, res, OutcomeStoreUnavailable, storeUnavailable())
	}
}

func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	var res Result
	if _, ok := g.authenticate(w, r, &res); !ok {
		return
	}
	data := []map[string]any{}
	for _, name := range g.models.ModelNames() {
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "hardcap"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// apiError is an error reply in OpenAI's format.
type apiError struct {
	status             int
	typ, code, message string
}

func badRequest(status int, msg string) *apiError {
	return &apiError{status: status, typ: "invalid_request_error", message: msg}
}

func storeUnavailable() *apiError {
	return &apiError{status: http.StatusServiceUnavailable, typ: "server_error", code: "budget_store_unavailable",
		message: "the budget store is unavailable, so this request can't be checked against its budget; " +
			"Hardcap fails closed and sent nothing to the AI provider"}
}

// fail sends an error reply and records it.
func (g *Gateway) fail(w http.ResponseWriter, res *Result, outcome string, e *apiError) {
	res.Status, res.Outcome = e.status, outcome
	body := map[string]any{"type": e.typ, "message": e.message}
	if e.code != "" {
		body["code"] = e.code
	}
	writeJSON(w, e.status, map[string]any{"error": body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func bearerToken(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

func serverTiming(name string, d time.Duration) string {
	return fmt.Sprintf("%s;dur=%.3f", name, float64(d.Microseconds())/1000)
}

func usdHeader(m pricing.Micros) string {
	return fmt.Sprintf("%.6f", m.USD())
}

// newRequestID returns an ID like "req_3f9a1c2b7d4e8f60", sent back in the
// X-Request-Id header and printed in the logs.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on supported platforms
	return "req_" + hex.EncodeToString(b[:])
}
