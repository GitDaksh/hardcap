// Package mock is a fake AI provider that speaks the OpenAI chat API.
//
// To Hardcap it looks exactly like OpenAI, Groq or Ollama, but it costs
// nothing, always answers the same way, and can be made slow or flaky on
// purpose. Every load test and crash test runs against it, so testing never
// spends real money.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Config controls how the fake provider behaves.
type Config struct {
	// TokenDelay is the pause before each token, imitating a real model
	// typing out its answer. Zero makes replies instant.
	TokenDelay time.Duration

	// FailRate is the chance (0 to 1) that a request fails with HTTP 500.
	FailRate float64

	// ReplyTokens is how long a full reply is. A request's max_tokens can
	// cut it shorter, never longer. Zero means one pass of the reply text.
	ReplyTokens int
}

// Usage is the token count that OpenAI-compatible providers report.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// maxReplyTokens stops one request from keeping the server busy forever.
const maxReplyTokens = 4096

// replyText is the fake model's whole vocabulary: one word is one token.
var replyText = strings.Fields("This is a fake reply from the Hardcap mock provider. " +
	"It streams one word at a time so you can test it for free.")

type server struct {
	cfg    Config
	nextID atomic.Int64
}

// NewHandler returns the fake provider's HTTP routes.
func NewHandler(cfg Config) http.Handler {
	if cfg.ReplyTokens <= 0 {
		cfg.ReplyTokens = len(replyText)
	}
	s := &server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// chatRequest holds the request fields the fake provider looks at.
type chatRequest struct {
	Model               string    `json:"model"`
	Messages            []message `json:"messages"`
	Stream              bool      `json:"stream"`
	MaxTokens           *int      `json:"max_tokens"`
	MaxCompletionTokens *int      `json:"max_completion_tokens"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // a string, or a list of parts
}

// completion is one fake answer, ready to send whole or as a stream.
type completion struct {
	id, model string
	tokens    []string
	finish    string // "stop": it finished on its own; "length": it hit max_tokens
	usage     Usage
}

func (s *server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
		return
	}
	if rand.Float64() < s.cfg.FailRate {
		writeError(w, http.StatusInternalServerError, "server_error", "mock: simulated provider failure")
		return
	}

	c := s.complete(req)
	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		s.sendStream(r.Context(), w, c, includeUsage)
		return
	}
	s.sendWhole(r.Context(), w, c)
}

// complete builds the fake answer for req.
func (s *server) complete(req chatRequest) completion {
	n, finish := s.cfg.ReplyTokens, "stop"
	limit := req.MaxTokens
	if req.MaxCompletionTokens != nil { // the newer name for the same limit
		limit = req.MaxCompletionTokens
	}
	if limit != nil && *limit < n {
		n, finish = max(*limit, 0), "length"
	}
	n = min(n, maxReplyTokens)

	tokens := make([]string, n)
	for i := range tokens {
		tokens[i] = replyText[i%len(replyText)]
		if i > 0 {
			tokens[i] = " " + tokens[i]
		}
	}

	model := req.Model
	if model == "" {
		model = "mock-1"
	}
	prompt := promptTokens(req.Messages)
	return completion{
		id:     fmt.Sprintf("chatcmpl-mock-%d", s.nextID.Add(1)),
		model:  model,
		tokens: tokens,
		finish: finish,
		usage:  Usage{PromptTokens: prompt, CompletionTokens: n, TotalTokens: prompt + n},
	}
}

// sendWhole "thinks" for the whole reply, then answers in one piece.
func (s *server) sendWhole(ctx context.Context, w http.ResponseWriter, c completion) {
	if !sleep(ctx, time.Duration(len(c.tokens))*s.cfg.TokenDelay) {
		return // the caller hung up
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      c.id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": strings.Join(c.tokens, "")},
			"finish_reason": c.finish,
		}},
		"usage": c.usage,
	})
}

// sendStream sends the reply the way OpenAI does: Server-Sent Events, one
// small JSON chunk per token, an optional usage chunk, then [DONE].
func (s *server) sendStream(ctx context.Context, w http.ResponseWriter, c completion, includeUsage bool) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	created := time.Now().Unix()
	send := func(choices []any, usage *Usage) bool {
		chunk := map[string]any{
			"id":      c.id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   c.model,
			"choices": choices,
		}
		if includeUsage {
			chunk["usage"] = usage // null on every chunk but the last, like OpenAI
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	delta := func(d map[string]string, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}
	}

	if !send(delta(map[string]string{"role": "assistant", "content": ""}, nil), nil) {
		return
	}
	for _, tok := range c.tokens {
		if !sleep(ctx, s.cfg.TokenDelay) || !send(delta(map[string]string{"content": tok}, nil), nil) {
			return // the caller hung up: stop "generating"
		}
	}
	if !send(delta(map[string]string{}, c.finish), nil) {
		return
	}
	if includeUsage && !send([]any{}, &c.usage) {
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

// promptTokens estimates the input's size with the usual rule of thumb,
// about 4 characters per token, plus a few tokens per message for the chat
// formatting, the way real providers count.
func promptTokens(msgs []message) int {
	total := 0
	for _, m := range msgs {
		text := string(m.Content)
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			text = s
		}
		total += (len(text)+3)/4 + 3
	}
	return max(total, 1)
}

// sleep waits for d, or returns false early if the caller hangs up first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": errType, "message": msg}})
}
