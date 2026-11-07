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
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/gateway"
	"github.com/GitDaksh/hardcap/internal/mock"
)

const (
	chatPath   = "/v1/chat/completions"
	askFive    = `{"model":"mock-1","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`
	streamFive = `{"model":"mock-1","messages":[{"role":"user","content":"hi"}],"max_tokens":5,"stream":true}`
)

func TestNonStreamingReplyPassesThrough(t *testing.T) {
	gw, results := startGateway(t, startMock(t, mock.Config{}), "")

	resp := post(t, context.Background(), gw+chatPath, askFive)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var reply struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Choices) == 0 || reply.Choices[0].Message.Content != "This is a fake reply" {
		t.Errorf("reply = %+v, want the mock's 5-token answer", reply)
	}

	res := waitResult(t, results)
	if res.Usage == nil || res.Usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v, want 5 completion tokens", res.Usage)
	}
	if id := resp.Header.Get("X-Request-Id"); id == "" || id != res.RequestID {
		t.Errorf("X-Request-Id = %q, want %q", id, res.RequestID)
	}
}

func TestStreamingMeasuresUsageButHidesTheExtraChunk(t *testing.T) {
	gw, results := startGateway(t, startMock(t, mock.Config{}), "")

	resp := post(t, context.Background(), gw+chatPath, streamFive)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	events := readEvents(t, resp.Body)

	// Role chunk + 5 tokens + finish chunk + [DONE]. No usage chunk: this
	// client never asked for one, so the gateway hides it.
	if len(events) != 8 || events[7] != "[DONE]" {
		t.Fatalf("got %d events, want 8 ending in [DONE]:\n%s", len(events), strings.Join(events, "\n"))
	}
	var text strings.Builder
	for _, e := range events[:7] {
		var chunk struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(e), &chunk); err != nil {
			t.Fatal(err)
		}
		if len(chunk.Choices) == 0 {
			t.Fatalf("client received a chunk with no choices: %s", e)
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
	}
	if text.String() != "This is a fake reply" {
		t.Errorf("streamed text = %q, want %q", text.String(), "This is a fake reply")
	}

	res := waitResult(t, results)
	if res.Usage == nil || res.Usage.CompletionTokens != 5 {
		t.Errorf("usage = %+v, want 5 completion tokens (the gateway must ask the provider for it)", res.Usage)
	}
}

func TestStreamingKeepsTheUsageChunkWhenTheClientAsks(t *testing.T) {
	gw, _ := startGateway(t, startMock(t, mock.Config{}), "")

	body := `{"model":"mock-1","messages":[{"role":"user","content":"hi"}],"max_tokens":5,` +
		`"stream":true,"stream_options":{"include_usage":true}}`
	events := readEvents(t, post(t, context.Background(), gw+chatPath, body).Body)
	if len(events) != 9 {
		t.Fatalf("got %d events, want 9 (including the usage chunk)", len(events))
	}
	if !strings.Contains(events[7], `"completion_tokens":5`) {
		t.Errorf("usage chunk = %s", events[7])
	}
}

func TestStreamIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release // hold the rest of the reply back
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	gw, _ := startGateway(t, upstream, "")
	t.Cleanup(sync.OnceFunc(func() { close(release) })) // runs before the servers shut down

	resp := post(t, context.Background(), gw+chatPath, streamFive)
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

func TestClientHangUpCancelsTheProviderCall(t *testing.T) {
	providerCanceled := make(chan struct{})
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // keep "generating" until someone hangs up
		close(providerCanceled)
	})
	gw, results := startGateway(t, upstream, "")

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	resp := post(t, ctx, gw+chatPath, streamFive)
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	hangUp()

	select {
	case <-providerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider call kept running after the client left: we'd keep paying for tokens nobody reads")
	}
	if res := waitResult(t, results); !res.ClientGone {
		t.Error("ClientGone = false, want true")
	}
}

func TestProviderCrashMidStreamIsReportedToTheClient(t *testing.T) {
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection, like a crashed provider
	})
	gw, results := startGateway(t, upstream, "")

	events := readEvents(t, post(t, context.Background(), gw+chatPath, streamFive).Body)
	if len(events) != 2 || !strings.Contains(events[1], "upstream_error") {
		t.Fatalf("events = %q, want the first chunk, then an upstream_error event", events)
	}
	if res := waitResult(t, results); !res.UpstreamFailed {
		t.Error("UpstreamFailed = false, want true")
	}
}

func TestProviderErrorsPassThrough(t *testing.T) {
	for name, body := range map[string]string{"whole": askFive, "stream": streamFive} {
		t.Run(name, func(t *testing.T) {
			gw, results := startGateway(t, startMock(t, mock.Config{FailRate: 1}), "")

			resp := post(t, context.Background(), gw+chatPath, body)
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(string(got), "simulated provider failure") {
				t.Fatalf("got %d %s, want the provider's own 500 error", resp.StatusCode, got)
			}
			if res := waitResult(t, results); res.Status != http.StatusInternalServerError || res.Usage != nil {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestUnreachableProviderIs502(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/v1"
	dead.Close() // nothing listens there any more

	gw, results := startGateway(t, deadURL, "")
	resp := post(t, context.Background(), gw+chatPath, askFive)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if res := waitResult(t, results); !res.UpstreamFailed {
		t.Error("UpstreamFailed = false, want true")
	}
}

func TestRejectsBadRequests(t *testing.T) {
	gw, _ := startGateway(t, startMock(t, mock.Config{}), "")
	huge := `{"model":"mock-1","messages":[{"role":"user","content":"` + strings.Repeat("a", 1<<20) + `"}]}`

	cases := []struct {
		name, body string
		want       int
	}{
		{"not JSON", `{nope`, http.StatusBadRequest},
		{"not an object", `[1, 2]`, http.StatusBadRequest},
		{"trailing junk", `{"model":"mock-1"} extra`, http.StatusBadRequest},
		{"no model", `{"messages":[]}`, http.StatusBadRequest},
		{"stream is not a bool", `{"model":"mock-1","stream":"yes"}`, http.StatusBadRequest},
		{"over 1 MiB", huge, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if resp := post(t, context.Background(), gw+chatPath, tc.body); resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}

	t.Run("GET", func(t *testing.T) {
		resp, err := http.Get(gw + chatPath)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})
}

func TestForwardsTheRequestFaithfully(t *testing.T) {
	type seen struct {
		path, auth string
		body       string
	}
	got := make(chan seen, 1)
	upstream := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.URL.Path, r.Header.Get("Authorization"), string(body)}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	gw, _ := startGateway(t, upstream, "provider-secret")

	req, err := http.NewRequest(http.MethodPost, gw+chatPath, strings.NewReader(
		`{"model":"mock-1","messages":[],"max_tokens":1000000,"temperature":0.7,"extra":{"keep":"me <&>"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var s seen
	select {
	case s = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider never received the request")
	}
	if s.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", s.path)
	}
	if s.auth != "Bearer provider-secret" {
		t.Errorf("Authorization = %q: the provider must get the gateway's key, never the client's", s.auth)
	}
	for _, want := range []string{`"max_tokens":1000000`, `"temperature":0.7`, `"extra":{"keep":"me <&>"}`} {
		if !strings.Contains(s.body, want) {
			t.Errorf("provider got %s, which is missing %s", s.body, want)
		}
	}
}

// startMock runs the fake provider and returns its base URL.
func startMock(t *testing.T, cfg mock.Config) string {
	t.Helper()
	srv := httptest.NewServer(mock.NewHandler(cfg))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// startUpstream runs a hand-written provider, for tests that need exact
// control over what the provider does and when.
func startUpstream(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// startGateway runs a gateway in front of upstreamURL. The Result of every
// finished request arrives on the returned channel.
func startGateway(t *testing.T, upstreamURL, upstreamKey string) (string, <-chan gateway.Result) {
	t.Helper()
	results := make(chan gateway.Result, 64)
	gw, err := gateway.New(gateway.Config{
		UpstreamURL: upstreamURL,
		UpstreamKey: upstreamKey,
		OnComplete:  func(r gateway.Result) { results <- r },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv.URL, results
}

// post sends body as JSON. The response body is closed when the test ends.
func post(t *testing.T, ctx context.Context, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
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

// waitResult returns the gateway's next Result, or fails after 2 seconds.
func waitResult(t *testing.T, results <-chan gateway.Result) gateway.Result {
	t.Helper()
	select {
	case res := <-results:
		return res
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the gateway to finish the request")
		return gateway.Result{}
	}
}