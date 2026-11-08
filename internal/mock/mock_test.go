package mock_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GitDaksh/hardcap/internal/mock"
)

const hi = `"messages":[{"role":"user","content":"hi"}]`

func TestMaxTokensCutsTheReplyShort(t *testing.T) {
	got := askWhole(t, mock.Config{}, `{"model":"mock-1",`+hi+`,"max_tokens":3}`)
	if got.text != "This is a" || got.finish != "length" || got.usage.CompletionTokens != 3 {
		t.Errorf("got %+v, want 3 tokens cut off by length", got)
	}
}

func TestFullReplyStopsOnItsOwn(t *testing.T) {
	got := askWhole(t, mock.Config{ReplyTokens: 2}, `{"model":"mock-1",`+hi+`,"max_tokens":50}`)
	if got.text != "This is" || got.finish != "stop" || got.usage.CompletionTokens != 2 {
		t.Errorf("got %+v, want a 2-token reply that stopped on its own", got)
	}
}

func TestStreamSendsUsageOnlyWhenAsked(t *testing.T) {
	srv := httptest.NewServer(mock.NewHandler(mock.Config{}))
	defer srv.Close()

	// Without stream_options: role chunk + 2 tokens + finish chunk + [DONE].
	plain := readEvents(t, post(t, srv.URL, `{"model":"mock-1",`+hi+`,"max_tokens":2,"stream":true}`).Body)
	if len(plain) != 5 || strings.Contains(strings.Join(plain, ""), "usage") {
		t.Fatalf("plain stream = %q, want 5 events and no usage", plain)
	}

	// With include_usage: one extra chunk, with no choices, carrying the usage.
	withUsage := readEvents(t, post(t, srv.URL,
		`{"model":"mock-1",`+hi+`,"max_tokens":2,"stream":true,"stream_options":{"include_usage":true}}`).Body)
	if len(withUsage) != 6 || withUsage[5] != "[DONE]" {
		t.Fatalf("usage stream = %q, want 6 events ending in [DONE]", withUsage)
	}
	var last struct {
		Choices []any       `json:"choices"`
		Usage   *mock.Usage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(withUsage[4]), &last); err != nil {
		t.Fatal(err)
	}
	if last.Usage == nil || last.Usage.CompletionTokens != 2 || len(last.Choices) != 0 {
		t.Errorf("usage chunk = %s", withUsage[4])
	}
}

func TestFailRateOneAlwaysFails(t *testing.T) {
	srv := httptest.NewServer(mock.NewHandler(mock.Config{FailRate: 1}))
	defer srv.Close()

	if resp := post(t, srv.URL, `{"model":"mock-1",`+hi+`}`); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestRejectsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(mock.NewHandler(mock.Config{}))
	defer srv.Close()

	if resp := post(t, srv.URL, `{nope`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

type wholeReply struct {
	text, finish string
	usage        mock.Usage
}

// askWhole sends a non-streaming request to a fresh mock and decodes the reply.
func askWhole(t *testing.T, cfg mock.Config, body string) wholeReply {
	t.Helper()
	srv := httptest.NewServer(mock.NewHandler(cfg))
	defer srv.Close()

	var reply struct {
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
		Usage mock.Usage `json:"usage"`
	}
	if err := json.NewDecoder(post(t, srv.URL, body).Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Choices) == 0 {
		t.Fatal("reply has no choices")
	}
	return wholeReply{reply.Choices[0].Message.Content, reply.Choices[0].FinishReason, reply.Usage}
}

// post sends body to the chat endpoint; the response is closed when the test ends.
func post(t *testing.T, baseURL, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
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
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
