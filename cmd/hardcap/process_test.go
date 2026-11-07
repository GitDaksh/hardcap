package main

// These tests run the real gateway as a separate process, then crash it
// (SIGKILL) or stop it (SIGTERM) in the middle of streamed requests, and
// check the books afterwards.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/mock"
	"github.com/GitDaksh/hardcap/internal/pricing"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

// When a test re-runs this test binary with HARDCAP_TEST_RUN_GATEWAY=1, the
// binary becomes the real gateway: main() instead of the tests.
func TestMain(m *testing.M) {
	if os.Getenv("HARDCAP_TEST_RUN_GATEWAY") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type world struct {
	t       *testing.T
	books   *ledger.Ledger
	project ledger.Project
	secret  string
	env     []string
	addr    string
}

// setup makes a fresh database, a project with a $1 cap, a mock provider
// whose replies take replyTokens x tokenDelay, and the gateway's environment.
func setup(t *testing.T, replyTokens int, tokenDelay time.Duration, extraEnv ...string) *world {
	t.Helper()
	if testing.Short() {
		t.Skip("starts the gateway as a separate process")
	}
	dbURL := testenv.PostgresURL(t)
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	books := ledger.New(pool)
	p, err := books.CreateProject(context.Background(), "crash-test", pricing.PerDollar, ledger.ModeHardcap)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := books.CreateKey(context.Background(), p.ID, ledger.KeyOptions{Name: "k"})
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(mock.NewHandler(mock.Config{TokenDelay: tokenDelay, ReplyTokens: replyTokens}))
	t.Cleanup(upstream.Close)
	cfg := filepath.Join(t.TempDir(), "hardcap.yaml")
	yaml := fmt.Sprintf(`
providers:
  mock: {base_url: "%s/v1"}
models:
  mock-1:
    default_max_tokens: %d
    prompt_overhead_tokens: 0
    routes: [{provider: mock, input_per_million: 10, output_per_million: 30}]
`, upstream.URL, replyTokens)
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	addr := freeAddr(t)
	env := append(os.Environ(),
		"HARDCAP_TEST_RUN_GATEWAY=1",
		"HARDCAP_ADDR="+addr,
		"HARDCAP_CONFIG="+cfg,
		"HARDCAP_DATABASE_URL="+dbURL,
		"HARDCAP_REDIS_URL=",
		"HARDCAP_ADMIN_TOKEN=",
		"HARDCAP_SWEEP_INTERVAL=100ms",
	)
	return &world{t: t, books: books, project: p, secret: secret, env: append(env, extraEnv...), addr: addr}
}

type gatewayProcess struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{} // closed when the process has exited
	err  error         // how it exited; read only after done is closed
}

// start launches the gateway and waits until it's ready.
func (w *world) start() *gatewayProcess {
	w.t.Helper()
	g := &gatewayProcess{cmd: exec.Command(os.Args[0]), out: &syncBuffer{}, done: make(chan struct{})}
	g.cmd.Env = w.env
	g.cmd.Stdout, g.cmd.Stderr = g.out, g.out
	if err := g.cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	go func() {
		g.err = g.cmd.Wait()
		close(g.done)
	}()
	w.t.Cleanup(func() {
		_ = g.cmd.Process.Kill()
		<-g.done
		if w.t.Failed() {
			w.t.Logf("gateway output:\n%s", g.out.String())
		}
	})
	w.waitFor(10*time.Second, "the gateway to be ready", func() bool {
		resp, err := http.Get("http://" + w.addr + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
	return g
}

// stream starts n streaming requests in the background. Each result is the
// HTTP status plus whether the stream ended with [DONE].
func (w *world) stream(n int) <-chan string {
	results := make(chan string, n)
	for range n {
		go func() {
			body := `{"model":"mock-1","messages":[{"role":"user","content":"hi"}],"stream":true}`
			req, _ := http.NewRequest(http.MethodPost, "http://"+w.addr+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+w.secret)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- "error"
				return
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			results <- fmt.Sprintf("%d done=%v", resp.StatusCode, err == nil && bytes.Contains(data, []byte("[DONE]")))
		}()
	}
	return results
}

func (w *world) audit() ledger.Audit {
	w.t.Helper()
	a, err := w.books.Audit(context.Background(), w.project.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return a
}

func (w *world) waitFor(limit time.Duration, what string, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out after %v waiting for %s", limit, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// kill -9 a gateway in the middle of 10 streams. Nothing settles, so the
// money stays held. A new gateway's sweeper finds the holds once they
// expire and charges them in full, because the provider may have generated
// every token. The cap still holds and the books still balance.
func TestCrashedGatewaysHoldsAreChargedInFull(t *testing.T) {
	w := setup(t, 200, 50*time.Millisecond, // replies take 10s
		"HARDCAP_REQUEST_TIMEOUT=2s", "HARDCAP_HOLD_GRACE=1s") // holds live 3s
	first := w.start()

	w.stream(10)
	w.waitFor(5*time.Second, "10 open holds", func() bool { return w.audit().OpenHolds == 10 })
	if err := first.cmd.Process.Kill(); err != nil { // SIGKILL: no shutdown, no settling
		t.Fatal(err)
	}
	<-first.done

	held := w.audit()
	if held.Project.Spent != 0 || held.Project.Reserved == 0 || held.OpenHolds != 10 {
		t.Fatalf("right after the crash: %+v", held)
	}

	w.start() // a fresh gateway; its sweeper does the cleanup
	w.waitFor(15*time.Second, "the orphaned holds to be charged", func() bool { return w.audit().OpenHolds == 0 })

	a := w.audit()
	if a.Project.Spent != held.Project.Reserved || a.Project.Reserved != 0 || !a.Balanced() {
		t.Fatalf("after the sweep: %+v; want everything that was held charged", a)
	}
	if a.Project.Spent > a.Project.Cap {
		t.Fatalf("CAP CROSSED: %+v", a)
	}
	events, err := w.books.RecentUsage(context.Background(), w.project.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Outcome != "expired" || e.Cost != e.Held {
			t.Errorf("receipt %+v: want outcome expired, charged in full", e)
		}
	}
}

// SIGTERM in the middle of 5 streams: the gateway stops taking new requests
// but lets the 5 finish and settle at their real cost before it exits.
func TestGracefulShutdownLetsRequestsFinishAndSettle(t *testing.T) {
	w := setup(t, 40, 25*time.Millisecond) // replies take 1s
	g := w.start()

	results := w.stream(5)
	w.waitFor(5*time.Second, "5 open holds", func() bool { return w.audit().OpenHolds == 5 })
	if err := g.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		select {
		case r := <-results:
			if r != "200 done=true" {
				t.Errorf("a request in flight during shutdown ended with %q, want a complete reply", r)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("requests didn't finish during the graceful shutdown")
		}
	}
	select {
	case <-g.done:
		if g.err != nil {
			t.Errorf("the gateway exited with %v, want a clean exit", g.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway didn't exit")
	}

	a := w.audit()
	if a.OpenHolds != 0 || a.ReceiptCount != 5 || !a.Balanced() {
		t.Fatalf("after shutdown: %+v; want 5 settled requests", a)
	}
	want := pricing.Price{InputPerMillion: 10_000_000, OutputPerMillion: 30_000_000}.Cost(4, 40)
	if a.Project.Spent != 5*want {
		t.Errorf("spent %s, want 5 x %s: each settled at its real cost, not its hold", a.Project.Spent, want)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// syncBuffer collects the gateway's output from its pipes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
