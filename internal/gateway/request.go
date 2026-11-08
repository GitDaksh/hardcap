package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"

	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/pricing"
)

// chatRequest is a parsed, checked chat request.
type chatRequest struct {
	payload          map[string]any // the client's JSON, with numbers kept exact
	model            *config.Model
	stream           bool
	clientWantsUsage bool // the client itself asked for the usage chunk
	maxTokens        int  // the output limit every provider will be told

	// promptBound is a guaranteed upper limit on the prompt's size in
	// tokens. Every token is at least one byte, so the request's byte count
	// can't be smaller than its token count. The model's prompt overhead
	// covers what the provider adds around the messages.
	promptBound int
}

// worstTokens is the most tokens this request can use.
func (r *chatRequest) worstTokens() int64 {
	return int64(r.promptBound) + int64(r.maxTokens)
}

// worstCost is the most this request can cost, on its most expensive route.
func (r *chatRequest) worstCost() pricing.Micros {
	return r.model.WorstPrice().Cost(int64(r.promptBound), int64(r.maxTokens))
}

// parseChatRequest reads and checks a request. Fields Hardcap doesn't know
// are kept and forwarded untouched.
func parseChatRequest(body []byte, models map[string]*config.Model) (*chatRequest, *apiError) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep 1000000 as 1000000, not 1e+06
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil || payload == nil {
		return nil, badRequest(http.StatusBadRequest, "request body must be a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, badRequest(http.StatusBadRequest, "request body must be a single JSON object")
	}

	name, _ := payload["model"].(string)
	if name == "" {
		return nil, badRequest(http.StatusBadRequest, "model is required")
	}
	model, ok := models[name]
	if !ok {
		return nil, &apiError{status: http.StatusNotFound, typ: "invalid_request_error", code: "model_not_found",
			message: fmt.Sprintf("model %q doesn't exist here; GET /v1/models lists the ones that do", name)}
	}
	req := &chatRequest{payload: payload, model: model}

	if v, ok := payload["stream"]; ok && v != nil {
		stream, isBool := v.(bool)
		if !isBool {
			return nil, badRequest(http.StatusBadRequest, "stream must be true or false")
		}
		req.stream = stream
	}
	if v, ok := payload["n"]; ok && v != nil {
		if n, err := positiveInt(v); err != nil || n != 1 {
			return nil, badRequest(http.StatusBadRequest, "n must be 1: Hardcap holds the cost of one reply per request")
		}
	}
	if e := checkMessages(payload["messages"]); e != nil {
		return nil, e
	}

	// Every request gets an output limit, because the cost of an unbounded
	// reply can't be capped. If the client set both fields, the smaller wins.
	for _, field := range []string{"max_completion_tokens", "max_tokens"} {
		v, ok := payload[field]
		if !ok || v == nil {
			continue
		}
		n, err := positiveInt(v)
		if err != nil {
			return nil, badRequest(http.StatusBadRequest, field+" must be a positive whole number")
		}
		if req.maxTokens == 0 || n < req.maxTokens {
			req.maxTokens = n
		}
	}
	if req.maxTokens == 0 {
		req.maxTokens = model.DefaultMaxTokens
	}
	if req.maxTokens > model.MaxOutputTokens {
		return nil, badRequest(http.StatusBadRequest,
			fmt.Sprintf("max_tokens can be at most %d for %s", model.MaxOutputTokens, model.Name))
	}

	if req.stream {
		opts, _ := payload["stream_options"].(map[string]any)
		req.clientWantsUsage, _ = opts["include_usage"].(bool)
	}
	req.promptBound = len(body) + model.PromptOverheadTokens
	return req, nil
}

// checkMessages makes sure every message is text. An image's token cost
// can't be bounded from the request's size, so Hardcap can't hold for it.
func checkMessages(v any) *apiError {
	msgs, ok := v.([]any)
	if !ok || len(msgs) == 0 {
		return badRequest(http.StatusBadRequest, "messages must be a non-empty list")
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			return badRequest(http.StatusBadRequest, "each message must be an object")
		}
		switch content := msg["content"].(type) {
		case nil, string: // nil is fine: e.g. an assistant message with only tool calls
		case []any:
			for _, part := range content {
				p, _ := part.(map[string]any)
				if p["type"] != "text" {
					return badRequest(http.StatusBadRequest,
						"only text content is supported: the cost of images and audio can't be bounded before the request runs")
				}
			}
		default:
			return badRequest(http.StatusBadRequest, "message content must be a string or a list of text parts")
		}
	}
	return nil
}

func positiveInt(v any) (int, error) {
	num, ok := v.(json.Number)
	if !ok {
		return 0, errors.New("not a number")
	}
	n, err := num.Int64()
	if err != nil || n < 1 || n > 1<<30 {
		return 0, errors.New("not a positive whole number")
	}
	return int(n), nil
}

// bodyFor builds the body for one provider: that provider's name for the
// model, its field for the output limit, and (for streams) a request for
// the usage report, without which a streamed reply's cost would be unknown.
func (r *chatRequest) bodyFor(upstreamModel, maxTokensField string) ([]byte, error) {
	p := maps.Clone(r.payload)
	p["model"] = upstreamModel
	delete(p, "max_tokens")
	delete(p, "max_completion_tokens")
	p[maxTokensField] = r.maxTokens
	if r.stream {
		opts := map[string]any{}
		if o, ok := r.payload["stream_options"].(map[string]any); ok {
			opts = maps.Clone(o)
		}
		opts["include_usage"] = true
		p["stream_options"] = opts
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <, > and & exactly as the client sent them
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
