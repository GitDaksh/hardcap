// Package gateway is the heart of Hardcap: an OpenAI-compatible reverse proxy.
//
// Apps send their AI requests here instead of straight to the provider.
// Because every request passes through, the gateway can measure exactly how
// many tokens each one used, and (from Phase 2) refuse any request that
// would push a key past its spending cap.
package gateway

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxRequestBytes  = 1 << 20  // 1 MiB is plenty for any chat request
	maxResponseBytes = 16 << 20 // the biggest non-streaming reply we'll hold in memory
)

var doneMarker = []byte("[DONE]")

// Config is everything needed to build a Gateway.
type Config struct {
	// UpstreamURL is the provider's OpenAI-compatible base URL, ending in
	// /v1. For the mock provider that's http://localhost:8090/v1.
	UpstreamURL string

	// UpstreamKey is the provider's API key. Only the gateway ever holds it;
	// apps never see it. Empty means "send no key" (the mock needs none).
	UpstreamKey string

	// Logger gets one line per request. Nil turns logging off.
	Logger *slog.Logger

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
	RequestID      string
	Model          string
	Stream         bool
	Status         int    // HTTP status sent to the client; 0 if none was sent
	Usage          *Usage // nil if the provider never reported usage
	Events         int    // stream events received from the provider
	ClientGone     bool   // the client hung up before the reply finished
	UpstreamFailed bool   // the provider was unreachable or broke off mid-reply
	Duration       time.Duration
}

// Gateway forwards chat requests to one upstream provider.
type Gateway struct {
	chatURL     string
	upstreamKey string
	client      *http.Client
	logger      *slog.Logger
	onComplete  func(Result)
}

// New checks cfg and builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	base, err := url.Parse(cfg.UpstreamURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("gateway: upstream URL %q should look like http://localhost:8090/v1", cfg.UpstreamURL)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// Go's default client keeps only 2 idle connections per host. A proxy
	// sends everything to one host, so under load it would keep opening new
	// TCP (and TLS) connections: slow, and it can run out of ports.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 512
	transport.MaxIdleConnsPerHost = 512
	// A non-streaming reply sends its headers only after the whole answer
	// is generated, which can legitimately take minutes.
	transport.ResponseHeaderTimeout = 5 * time.Minute

	return &Gateway{
		chatURL:     base.JoinPath("chat/completions").String(),
		upstreamKey: cfg.UpstreamKey,
		// No overall timeout: a stream can run for minutes. Provider calls
		// are canceled through the client's context instead.
		client:     &http.Client{Transport: transport},
		logger:     logger,
		onComplete: cfg.OnComplete,
	}, nil
}

// Handler returns the gateway's HTTP routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chatCompletions)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func (g *Gateway) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	res := Result{RequestID: newRequestID()}
	w.Header().Set("X-Request-Id", res.RequestID)
	defer func() {
		res.Duration = time.Since(start)
		g.finish(res)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, &res, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is larger than 1 MiB")
		} else {
			fail(w, &res, http.StatusBadRequest, "invalid_request_error", "could not read the request body")
		}
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		fail(w, &res, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	res.Model, res.Stream = req.model, req.stream

	// The provider call shares the client's context: if the client hangs
	// up, the provider call is canceled too, so nobody pays for tokens that
	// no one will read.
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.chatURL, bytes.NewReader(req.upstreamBody))
	if err != nil {
		fail(w, &res, http.StatusInternalServerError, "server_error", "could not build the provider request")
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", "application/json")
	if req.stream {
		up.Header.Set("Accept", "text/event-stream")
	}
	if g.upstreamKey != "" {
		up.Header.Set("Authorization", "Bearer "+g.upstreamKey)
	}

	resp, err := g.client.Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			res.ClientGone = true
			return
		}
		res.UpstreamFailed = true
		g.logger.Warn("provider unreachable", "request_id", res.RequestID, "error", err)
		fail(w, &res, http.StatusBadGateway, "upstream_error", "could not reach the AI provider")
		return
	}
	defer resp.Body.Close()

	if req.stream && resp.StatusCode == http.StatusOK && isEventStream(resp) {
		g.relayStream(w, r, resp, req.clientWantsUsage, &res)
		return
	}
	g.relayWhole(w, r, resp, &res)
}

// chatRequest is what the gateway needs to know about an incoming request.
type chatRequest struct {
	model            string
	stream           bool
	clientWantsUsage bool   // the client itself asked for the usage chunk
	upstreamBody     []byte // the body to forward to the provider
}

// parseChatRequest reads a request without losing anything: fields the
// gateway doesn't know are forwarded untouched, and numbers keep their exact
// original form (no 1000000 turning into 1e+06 on the way through).
func parseChatRequest(body []byte) (chatRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil || payload == nil {
		return chatRequest{}, errors.New("request body must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return chatRequest{}, errors.New("request body must be a single JSON object")
	}

	var req chatRequest
	req.model, _ = payload["model"].(string)
	if req.model == "" {
		return chatRequest{}, errors.New("model is required")
	}
	if v, ok := payload["stream"]; ok && v != nil {
		stream, isBool := v.(bool)
		if !isBool {
			return chatRequest{}, errors.New("stream must be true or false")
		}
		req.stream = stream
	}

	if req.stream {
		opts, _ := payload["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		req.clientWantsUsage, _ = opts["include_usage"].(bool)
		// Ask the provider to report token usage at the end of the stream.
		// Without it a streamed reply's cost is unknown, and Hardcap can't
		// cap what it can't measure.
		opts["include_usage"] = true
		payload["stream_options"] = opts
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <, > and & exactly as the client sent them
	if err := enc.Encode(payload); err != nil {
		return chatRequest{}, fmt.Errorf("could not re-encode the request: %w", err)
	}
	req.upstreamBody = buf.Bytes()
	return req, nil
}

// relayWhole forwards a complete (non-streaming) reply and reads its usage.
// Error replies from the provider are passed through unchanged.
func (g *Gateway) relayWhole(w http.ResponseWriter, r *http.Request, resp *http.Response, res *Result) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	switch {
	case err != nil && r.Context().Err() != nil:
		res.ClientGone = true
		return
	case err != nil:
		res.UpstreamFailed = true
		fail(w, res, http.StatusBadGateway, "upstream_error", "the AI provider broke off the reply")
		return
	case len(body) > maxResponseBytes:
		fail(w, res, http.StatusBadGateway, "upstream_error", "the AI provider's reply was too large")
		return
	}

	if resp.StatusCode == http.StatusOK {
		var reply struct {
			Usage *Usage `json:"usage"`
		}
		if json.Unmarshal(body, &reply) == nil {
			res.Usage = reply.Usage
		}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	res.Status = resp.StatusCode
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		res.ClientGone = true
	}
}

// relayStream forwards a streamed reply one event at a time, flushing each
// event to the client the moment it arrives, and picks out the usage report.
func (g *Gateway) relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, clientWantsUsage bool, res *Result) {
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // asks nginx-style proxies not to buffer
	res.Status = http.StatusOK
	w.WriteHeader(http.StatusOK)

	// send writes one event and flushes it. False means the client is gone.
	send := func(event []byte) bool {
		if _, err := w.Write(event); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(nil) { // get the headers to the client right away
		res.ClientGone = true
		return
	}

	reader := bufio.NewReaderSize(resp.Body, 32<<10)
	var event []byte // the lines of the event being read
	for {
		line, err := reader.ReadBytes('\n')
		event = append(event, line...)

		// A blank line ends an event, and so does the end of the stream.
		if isBlank(line) || errors.Is(err, io.EOF) {
			if len(bytes.TrimSpace(event)) > 0 {
				if out, keep := inspectEvent(event, clientWantsUsage, res); keep && !send(out) {
					res.ClientGone = true
					return
				}
			}
			event = event[:0]
		}

		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF):
			return // the provider finished normally
		case r.Context().Err() != nil:
			res.ClientGone = true
			return
		default:
			// The provider died mid-reply. The 200 status has already gone
			// out, so tell the client in the stream itself, like OpenAI does.
			res.UpstreamFailed = true
			g.logger.Warn("provider broke off the stream", "request_id", res.RequestID, "error", err)
			send([]byte(`data: {"error":{"type":"upstream_error","message":"the AI provider broke off the reply"}}` + "\n\n"))
			return
		}
	}
}

// inspectEvent records the usage report if this event carries one, and
// returns the event to forward plus whether to forward it at all.
func inspectEvent(event []byte, clientWantsUsage bool, res *Result) ([]byte, bool) {
	if data, ok := eventData(event); ok && !bytes.Equal(data, doneMarker) {
		res.Events++
		if mayHaveUsage(data) {
			var chunk struct {
				Choices []json.RawMessage `json:"choices"`
				Usage   *Usage            `json:"usage"`
			}
			if json.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
				res.Usage = chunk.Usage
				// This chunk only exists because the gateway asked for it. A
				// client that didn't ask may not expect a chunk with no
				// choices (some crash on it), so it never sees it.
				if len(chunk.Choices) == 0 && !clientWantsUsage {
					return nil, false
				}
			}
		}
	}
	// Normalize the ending to exactly one blank line.
	return append(bytes.TrimRight(event, "\r\n"), '\n', '\n'), true
}

// eventData joins the payloads of an event's "data:" lines, as the SSE spec
// says. ok is false when the event has no data lines at all.
func eventData(event []byte) (data []byte, ok bool) {
	for _, line := range bytes.Split(event, []byte("\n")) {
		payload, found := bytes.CutPrefix(bytes.TrimSuffix(line, []byte("\r")), []byte("data:"))
		if !found {
			continue
		}
		if ok {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimPrefix(payload, []byte(" "))...)
		ok = true
	}
	return data, ok
}

// mayHaveUsage is a cheap check before a full JSON parse. With include_usage
// on, every chunk says "usage":null and only the last one has numbers, so
// most chunks can skip parsing entirely.
func mayHaveUsage(data []byte) bool {
	i := bytes.Index(data, []byte(`"usage"`))
	if i < 0 {
		return false
	}
	rest := bytes.TrimLeft(data[i+len(`"usage"`):], " \t\r\n:")
	return !bytes.HasPrefix(rest, []byte("null"))
}

func isBlank(line []byte) bool {
	return len(line) > 0 && len(bytes.TrimRight(line, "\r\n")) == 0
}

func isEventStream(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}

// finish logs the request and hands its Result to OnComplete.
func (g *Gateway) finish(res Result) {
	attrs := []any{
		"request_id", res.RequestID,
		"model", res.Model,
		"stream", res.Stream,
		"status", res.Status,
		"duration_ms", res.Duration.Milliseconds(),
	}
	switch {
	case res.Usage != nil:
		attrs = append(attrs, "prompt_tokens", res.Usage.PromptTokens, "completion_tokens", res.Usage.CompletionTokens)
	case res.Status == http.StatusOK && !res.ClientGone:
		attrs = append(attrs, "usage", "missing")
	}
	if res.ClientGone {
		attrs = append(attrs, "client_gone", true)
	}
	if res.UpstreamFailed {
		attrs = append(attrs, "upstream_failed", true)
	}
	g.logger.Info("request finished", attrs...)
	if g.onComplete != nil {
		g.onComplete(res)
	}
}

// fail sends an OpenAI-style error and records its status.
func fail(w http.ResponseWriter, res *Result, status int, errType, msg string) {
	res.Status = status
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": errType, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newRequestID returns an ID like "req_3f9a1c2b7d4e8f60", sent back to the
// client in the X-Request-Id header and printed in the logs.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on supported platforms
	return "req_" + hex.EncodeToString(b[:])
}