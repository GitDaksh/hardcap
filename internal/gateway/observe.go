package gateway

import (
	"strconv"

	"github.com/GitDaksh/hardcap/internal/metrics"
)

// gatewayMetrics are the numbers the gateway publishes at /metrics.
type gatewayMetrics struct {
	requests         *metrics.CounterVec
	duration         *metrics.HistogramVec
	admission        *metrics.HistogramVec
	inFlight         *metrics.GaugeVec
	spend            *metrics.CounterVec
	held             *metrics.CounterVec
	attempts         *metrics.CounterVec
	breakerState     *metrics.GaugeVec
	budgetRejections *metrics.CounterVec
	rateLimitErrors  *metrics.CounterVec
	settleErrors     *metrics.CounterVec
	holdExceeded     *metrics.CounterVec
}

func newGatewayMetrics(r *metrics.Registry) *gatewayMetrics {
	return &gatewayMetrics{
		requests: r.Counter("hardcap_requests_total",
			"Chat requests by model and outcome.", "model", "outcome"),
		duration: r.Histogram("hardcap_request_duration_seconds",
			"Total time per chat request, including the provider.", metrics.DefaultLatencyBuckets, "model", "stream"),
		admission: r.Histogram("hardcap_admission_duration_seconds",
			"Hardcap's own overhead per request: auth, rate limit and budget hold.",
			[]float64{.0005, .001, .002, .005, .01, .025, .05, .1, .25, .5, 1}),
		inFlight: r.Gauge("hardcap_requests_in_flight",
			"Chat requests being handled right now."),
		spend: r.Counter("hardcap_spend_micros_total",
			"Money charged, in micro-dollars.", "model"),
		held: r.Counter("hardcap_held_micros_total",
			"Money held before requests ran, in micro-dollars.", "model"),
		attempts: r.Counter("hardcap_upstream_attempts_total",
			"Calls to providers by result: success, client_error, retryable, ambiguous or breaker_open.", "provider", "result"),
		breakerState: r.Gauge("hardcap_breaker_state",
			"Circuit breaker state per provider: 0 closed, 1 half-open, 2 open.", "provider"),
		budgetRejections: r.Counter("hardcap_budget_rejections_total",
			"Requests refused because they didn't fit under a cap.", "scope"),
		rateLimitErrors: r.Counter("hardcap_ratelimit_errors_total",
			"Rate limit checks that failed (Redis down); those requests were let through."),
		settleErrors: r.Counter("hardcap_settle_errors_total",
			"Settles that failed; their holds get charged in full when they expire."),
		holdExceeded: r.Counter("hardcap_hold_exceeded_total",
			"Requests whose real cost came out above their hold (should stay at 0)."),
	}
}

// finish logs a finished request and records its metrics.
func (g *Gateway) finish(res Result) {
	model := res.Model
	if model == "" {
		model = "none"
	}
	g.m.requests.With(model, res.Outcome).Inc()
	g.m.duration.With(model, strconv.FormatBool(res.Stream)).Observe(res.Duration.Seconds())
	if res.Admission > 0 {
		g.m.admission.With().Observe(res.Admission.Seconds())
	}
	if res.Held > 0 {
		g.m.held.With(model).Add(float64(res.Held))
	}
	if res.Cost > 0 {
		g.m.spend.With(model).Add(float64(res.Cost))
	}

	attrs := []any{
		"request_id", res.RequestID,
		"key_id", res.KeyID,
		"model", res.Model,
		"provider", res.Provider,
		"stream", res.Stream,
		"status", res.Status,
		"outcome", res.Outcome,
		"held", res.Held.String(),
		"cost", res.Cost.String(),
		"duration_ms", res.Duration.Milliseconds(),
	}
	if res.Usage != nil {
		attrs = append(attrs, "prompt_tokens", res.Usage.PromptTokens, "completion_tokens", res.Usage.CompletionTokens)
	}
	if res.Attempts > 1 {
		attrs = append(attrs, "attempts", res.Attempts)
	}
	g.logger.Info("request finished", attrs...)
	if g.onComplete != nil {
		g.onComplete(res)
	}
}
