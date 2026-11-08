// Command stampede is Hardcap's load test and proof. It creates a project
// with a small cap, sends a crowd of concurrent clients at it until the
// budget runs dry, then audits the books.
//
// Run it against both modes to see the difference:
//
//	go run ./cmd/stampede -mode both
//
// It exits with status 1 if a Hardcap-mode project ever ends above its cap
// or its books don't balance, so it can gate CI.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GitDaksh/hardcap/internal/loadgen"
)

func main() {
	url := flag.String("url", "http://localhost:8080", "the Hardcap gateway")
	token := flag.String("token", os.Getenv("HARDCAP_ADMIN_TOKEN"), "admin token (default: $HARDCAP_ADMIN_TOKEN)")
	mode := flag.String("mode", "both", "hardcap, naive, or both")
	capUSD := flag.String("cap", "1.00", "the project's cap in US dollars")
	workers := flag.Int("workers", 200, "clients sending requests at the same time")
	model := flag.String("model", "mock-1", "model to request (keep the free mock unless you mean to spend quota)")
	maxTokens := flag.Int("max-tokens", 0, "max_tokens per request (0: the model's default)")
	timeout := flag.Duration("timeout", 2*time.Minute, "stop after this long even if the budget isn't used up")
	flag.Parse()

	if *token == "" {
		fail("set HARDCAP_ADMIN_TOKEN or pass -token")
	}
	if v, err := strconv.ParseFloat(*capUSD, 64); err != nil || v <= 0 {
		fail("-cap must be a number of dollars above zero, like 1.00")
	}
	modes := []string{*mode}
	if *mode == "both" {
		modes = []string{"naive", "hardcap"}
	} else if *mode != "hardcap" && *mode != "naive" {
		fail("-mode must be hardcap, naive or both")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	admin := &adminClient{url: strings.TrimRight(*url, "/"), token: *token}

	fmt.Printf("Stampede: %d clients against a $%s cap, model %s\n\n", *workers, *capUSD, *model)
	var runs []result
	for _, m := range modes {
		fmt.Printf("Running %s mode...\n", m)
		r, err := stampede(ctx, admin, m, *capUSD, loadgen.Config{
			URL: admin.url, Model: *model, Workers: *workers, MaxTokens: *maxTokens,
			Stream: true, Duration: *timeout, StopAfterRefusals: 10,
		})
		if err != nil {
			fail(err.Error())
		}
		runs = append(runs, r)
	}
	fmt.Println()
	report(os.Stdout, runs)

	for _, r := range runs {
		if r.mode == "hardcap" && (r.audit.Project.Spent > r.audit.Project.Cap || !r.audit.Balanced) {
			fmt.Println("\nFAIL: a Hardcap project ended above its cap, or its books don't balance.")
			os.Exit(1)
		}
	}
}

type result struct {
	mode    string
	elapsed time.Duration
	stats   loadgen.Snapshot
	audit   auditView
}

type auditView struct {
	Project struct {
		ID       int64 `json:"id"`
		Cap      int64 `json:"cap_micros"`
		Spent    int64 `json:"spent_micros"`
		Reserved int64 `json:"reserved_micros"`
	} `json:"project"`
	ReceiptCount int64 `json:"receipt_count"`
	Balanced     bool  `json:"-"` // filled in from the response's top-level "balanced"
}

func stampede(ctx context.Context, admin *adminClient, mode, capUSD string, cfg loadgen.Config) (result, error) {
	var project struct {
		ID int64 `json:"id"`
	}
	name := fmt.Sprintf("stampede %s %s", mode, time.Now().Format("15:04:05.000"))
	if err := admin.call("POST", "/admin/api/projects", fmt.Sprintf(`{"name":%q,"cap_usd":%s,"mode":%q}`, name, capUSD, mode), &project); err != nil {
		return result{}, err
	}
	var key struct {
		Secret string `json:"secret"`
	}
	// No rate limits on this key: the stampede tests the budget, not the limiter.
	if err := admin.call("POST", fmt.Sprintf("/admin/api/projects/%d/keys", project.ID),
		`{"name":"stampede","rpm_limit":0,"tpm_limit":0}`, &key); err != nil {
		return result{}, err
	}

	cfg.APIKey = key.Secret
	var stats loadgen.Stats
	start := time.Now()
	loadgen.Run(ctx, cfg, &stats)
	elapsed := time.Since(start)

	var a struct {
		Audit    auditView `json:"audit"`
		Balanced bool      `json:"balanced"`
	}
	if err := admin.call("GET", fmt.Sprintf("/admin/api/projects/%d/audit", project.ID), "", &a); err != nil {
		return result{}, err
	}
	a.Audit.Balanced = a.Balanced
	return result{mode: mode, elapsed: elapsed, stats: stats.Snapshot(), audit: a.Audit}, nil
}

func report(out io.Writer, runs []result) {
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', tabwriter.AlignRight)
	row := func(label string, cell func(r result) string) {
		fmt.Fprint(w, label+"\t")
		for _, r := range runs {
			fmt.Fprint(w, cell(r)+"\t")
		}
		fmt.Fprintln(w)
	}
	row("", func(r result) string { return r.mode })
	row("requests sent", func(r result) string { return count(r.stats.Sent) })
	row("served (200)", func(r result) string { return count(r.stats.OK) })
	row("refused: over budget (402)", func(r result) string { return count(r.stats.Refused) })
	row("rate limited (429)", func(r result) string { return count(r.stats.RateLimited) })
	row("failed", func(r result) string { return count(r.stats.Failed) })
	row("spent", func(r result) string { return usd(r.audit.Project.Spent) })
	row("cap", func(r result) string { return usd(r.audit.Project.Cap) })
	row("over the cap", func(r result) string {
		over := r.audit.Project.Spent - r.audit.Project.Cap
		if over <= 0 {
			return "never"
		}
		return fmt.Sprintf("+%s (%.1f%%)", usd(over), 100*float64(over)/float64(r.audit.Project.Cap))
	})
	row("books balance", func(r result) string { return yesNo(r.audit.Balanced && r.audit.Project.Reserved == 0) })
	row("latency p50 / p99", func(r result) string { return fmt.Sprintf("%.0f / %.0f ms", r.stats.P50ms, r.stats.P99ms) })
	row("Hardcap overhead p50 / p99", func(r result) string {
		return fmt.Sprintf("%.2f / %.2f ms", r.stats.AdmitP50ms, r.stats.AdmitP99ms)
	})
	row("ran for", func(r result) string { return r.elapsed.Round(100 * time.Millisecond).String() })
	_ = w.Flush()
	for _, r := range runs {
		if r.stats.LastError != "" {
			fmt.Fprintf(out, "\nlast error seen in %s mode: %s\n", r.mode, r.stats.LastError)
		}
	}
}

type adminClient struct{ url, token string }

func (a *adminClient) call(method, path, body string, out any) error {
	req, err := http.NewRequest(method, a.url+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Hardcap at %s (is it running?): %w", a.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if resp.StatusCode == http.StatusNotFound && strings.HasPrefix(path, "/admin") && e.Error == "" {
			return errors.New("the admin API is off: start Hardcap with HARDCAP_ADMIN_TOKEN set")
		}
		return fmt.Errorf("%s %s: HTTP %d %s", method, path, resp.StatusCode, e.Error)
	}
	return json.Unmarshal(raw, out)
}

func usd(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

func count(n int64) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "stampede:", msg)
	os.Exit(2)
}
