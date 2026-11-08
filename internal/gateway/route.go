package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/upstream"
)

const (
	backoffBase = 100 * time.Millisecond
	backoffMax  = 2 * time.Second
	// A provider that asks us to wait longer than this (Retry-After) is
	// skipped for this request in favour of the next one.
	maxRetryWait = 2 * time.Second
)

// attempt is the provider call that will answer the client.
type attempt struct {
	resp     *http.Response
	route    config.Route
	provider *provider
	report   func(success bool) // tells the provider's circuit breaker how it went
}

// failedReply is a provider's error reply, kept to pass on if nothing works.
type failedReply struct {
	status int
	header http.Header
	body   []byte
}

// unavailableError means no provider took the request. None of them
// generated anything, so it costs nothing.
type unavailableError struct {
	last    *failedReply // the last provider error reply, if any
	reasons []string
}

func (e *unavailableError) Error() string {
	return "no provider could take this request: " + strings.Join(e.reasons, "; ")
}

// ambiguousError means a provider may have started generating before
// failing. It isn't retried, and the request is charged its full hold.
type ambiguousError struct {
	provider string
	err      error
}

func (e *ambiguousError) Error() string { return e.provider + ": " + e.err.Error() }
func (e *ambiguousError) Unwrap() error { return e.err }

// dial tries the model's routes in order until one answers.
//
// The safety rule: retry or fall back only after a failure that proves the
// provider generated nothing (connection refused, HTTP 429 or 5xx). After an
// ambiguous failure, like a timeout, the provider may already be generating,
// and a second try could spend more than the hold, so dial stops.
func (g *Gateway) dial(ctx context.Context, req *chatRequest, res *Result) (*attempt, error) {
	var last *failedReply
	var reasons []string
	for _, route := range req.model.Routes {
		p := g.providers[route.Provider]
		for try := 1; try <= g.models.Routing.AttemptsPerRoute; try++ {
			if err := ctx.Err(); err != nil {
				// Gone before anything was sent: nothing to pay for.
				return nil, &unavailableError{last: last, reasons: append(reasons, "request ended: "+err.Error())}
			}
			report, ok := p.breaker.Allow()
			if !ok {
				g.m.attempts.With(p.name, "breaker_open").Inc()
				reasons = append(reasons, p.name+": circuit breaker open")
				break // next route
			}
			body, err := req.bodyFor(route.Model, p.cfg.MaxTokensField)
			if err != nil {
				report(true)
				return nil, &unavailableError{reasons: []string{"could not build the request: " + err.Error()}}
			}

			res.Attempts++
			resp, err := g.send(ctx, p, body)
			verdict := upstream.Classify(resp, err)
			g.m.attempts.With(p.name, verdict.String()).Inc()

			switch verdict {
			case upstream.Success:
				return &attempt{resp: resp, route: route, provider: p, report: report}, nil
			case upstream.ClientError:
				report(true) // the provider is fine; the request was the problem
				return &attempt{resp: resp, route: route, provider: p, report: func(bool) {}}, nil
			case upstream.Ambiguous:
				report(false)
				res.maybeGenerated = true
				res.Provider = p.name
				return nil, &ambiguousError{provider: p.name, err: err}
			}

			// Retryable: nothing was generated, so another try is safe.
			report(false)
			wait := upstream.Backoff(try, backoffBase, backoffMax)
			if resp != nil {
				reasons = append(reasons, fmt.Sprintf("%s: HTTP %d", p.name, resp.StatusCode))
				if ra, ok := upstream.RetryAfter(resp.Header, time.Now()); ok {
					wait = ra
				}
				last = readFailedReply(resp)
			} else {
				reasons = append(reasons, fmt.Sprintf("%s: %v", p.name, err))
			}
			if try == g.models.Routing.AttemptsPerRoute || wait > maxRetryWait {
				break // next route
			}
			if !sleep(ctx, wait) {
				return nil, &unavailableError{last: last, reasons: append(reasons, "request ended while waiting to retry")}
			}
		}
	}
	return nil, &unavailableError{last: last, reasons: reasons}
}

func (g *Gateway) send(ctx context.Context, p *provider, body []byte) (*http.Response, error) {
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, p.chatURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "application/json, text/event-stream")
	if p.cfg.APIKey != "" {
		// Only the gateway holds provider keys. Clients never see them.
		up.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	return g.client.Do(up)
}

// failUpstream answers when dial found no provider.
func (g *Gateway) failUpstream(w http.ResponseWriter, r *http.Request, err error, res *Result) {
	if r.Context().Err() != nil {
		res.ClientGone = true
		res.Outcome = OutcomeClientGone
		return // nobody left to answer
	}
	var amb *ambiguousError
	var un *unavailableError
	switch {
	case errors.As(err, &amb):
		res.UpstreamFailed = true
		res.Outcome = OutcomeUpstreamFailed
		g.logger.Warn("provider failed mid-request; charging the full hold", "request_id", res.RequestID, "error", err)
		if errors.Is(err, context.DeadlineExceeded) {
			res.Status = http.StatusGatewayTimeout
			writeJSON(w, http.StatusGatewayTimeout, errorBody("upstream_error", "upstream_timeout", "the AI provider took too long to answer"))
			return
		}
		res.Status = http.StatusBadGateway
		writeJSON(w, http.StatusBadGateway, errorBody("upstream_error", "upstream_failed", "the AI provider failed while answering"))
	case errors.As(err, &un) && un.last != nil:
		// Pass on the provider's own last error: it says what went wrong
		// (and, for a 429, when to try again) better than we can.
		res.Outcome = OutcomeProviderError
		res.Status = un.last.status
		for _, h := range []string{"Content-Type", "Retry-After"} {
			if v := un.last.header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(un.last.status)
		_, _ = w.Write(un.last.body)
	default:
		res.Outcome = OutcomeUnavailable
		res.Status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusServiceUnavailable, errorBody("server_error", "no_provider_available", err.Error()))
	}
}

func errorBody(typ, code, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"type": typ, "code": code, "message": msg}}
}

// readFailedReply keeps a provider's error reply (up to 64 KiB) and closes it.
func readFailedReply(resp *http.Response) *failedReply {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	return &failedReply{status: resp.StatusCode, header: resp.Header.Clone(), body: body}
}

// sleep waits for d, or returns false early if ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
