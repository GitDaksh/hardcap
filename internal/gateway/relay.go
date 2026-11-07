package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/GitDaksh/hardcap/internal/upstream"
)

var doneMarker = []byte("[DONE]")

// relayWhole forwards a complete (non-streaming) reply and reads its usage.
// Error replies from the provider are passed through unchanged.
func (g *Gateway) relayWhole(w http.ResponseWriter, r *http.Request, resp *http.Response, res *Result) {
	verdict := upstream.Classify(resp, nil)
	if verdict == upstream.Success {
		res.maybeGenerated = true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	switch {
	case err != nil && r.Context().Err() != nil:
		res.ClientGone, res.Outcome = true, OutcomeClientGone
		return
	case err != nil:
		res.UpstreamFailed, res.Outcome = true, OutcomeUpstreamFailed
		res.Status = http.StatusBadGateway
		writeJSON(w, http.StatusBadGateway, errorBody("upstream_error", "upstream_failed", "the AI provider broke off the reply"))
		return
	case len(body) > maxResponseBytes:
		res.UpstreamFailed, res.Outcome = true, OutcomeUpstreamFailed
		res.Status = http.StatusBadGateway
		writeJSON(w, http.StatusBadGateway, errorBody("upstream_error", "reply_too_large", "the AI provider's reply was too large"))
		return
	}

	if verdict == upstream.Success {
		var reply struct {
			Usage *Usage `json:"usage"`
		}
		if json.Unmarshal(body, &reply) == nil && reply.Usage != nil {
			res.Usage = reply.Usage
			cost := res.route.Price().Cost(int64(reply.Usage.PromptTokens), int64(reply.Usage.CompletionTokens))
			w.Header().Set("X-Hardcap-Cost-Usd", usdHeader(cost))
		}
	} else {
		res.Outcome = OutcomeProviderError
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
	res.maybeGenerated = true
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
		res.ClientGone, res.Outcome = true, OutcomeClientGone
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
					res.ClientGone, res.Outcome = true, OutcomeClientGone
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
			res.ClientGone, res.Outcome = true, OutcomeClientGone
			return
		default:
			// The provider died (or timed out) mid-reply. The 200 status has
			// already gone out, so say so in the stream itself, like OpenAI.
			res.UpstreamFailed, res.Outcome = true, OutcomeUpstreamFailed
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
				XGroq   *struct {
					Usage *Usage `json:"usage"`
				} `json:"x_groq"` // Groq also reports usage here
			}
			if json.Unmarshal(data, &chunk) == nil {
				usage := chunk.Usage
				if usage == nil && chunk.XGroq != nil {
					usage = chunk.XGroq.Usage
				}
				if usage != nil {
					res.Usage = usage
					// This chunk only exists because the gateway asked for it.
					// A client that didn't ask may not expect a chunk with no
					// choices (some crash on it), so it never sees it.
					if len(chunk.Choices) == 0 && !clientWantsUsage {
						return nil, false
					}
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

// mayHaveUsage is a cheap check before a full JSON parse. With
// include_usage on, every chunk says "usage":null and only the last one has
// numbers, so most chunks can skip parsing entirely.
func mayHaveUsage(data []byte) bool {
	rest := data
	for {
		i := bytes.Index(rest, []byte(`"usage"`))
		if i < 0 {
			return false
		}
		rest = rest[i+len(`"usage"`):]
		if !bytes.HasPrefix(bytes.TrimLeft(rest, " \t\r\n:"), []byte("null")) {
			return true
		}
	}
}

func isBlank(line []byte) bool {
	return len(line) > 0 && len(bytes.TrimRight(line, "\r\n")) == 0
}

func isEventStream(resp *http.Response) bool {
	return strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
}
