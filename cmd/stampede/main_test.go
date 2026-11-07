package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/admin"
	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/gateway"
	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/loadgen"
	"github.com/GitDaksh/hardcap/internal/mock"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

// The CLI end to end, in one process: a mock provider, a real gateway and
// admin API on one server, and a stampede in each mode against a 5-cent cap.
func TestStampedeBothModes(t *testing.T) {
	books := ledger.New(testenv.Postgres(t))
	upstream := httptest.NewServer(mock.NewHandler(mock.Config{ReplyTokens: 5, TokenDelay: time.Millisecond}))
	defer upstream.Close()

	models, _, err := config.Parse(fmt.Appendf(nil, `
providers: {mock: {base_url: "%s/v1"}}
models:
  mock-1:
    default_max_tokens: 5
    prompt_overhead_tokens: 0
    routes: [{provider: mock, input_per_million: 10, output_per_million: 30}]
`, upstream.URL), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Config{Models: models, Ledger: books})
	if err != nil {
		t.Fatal(err)
	}
	adm, err := admin.New(admin.Config{Ledger: books, Token: "test-admin-token", Models: models.ModelNames()})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", gw.Handler())
	mux.Handle("/", adm.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &adminClient{url: srv.URL, token: "test-admin-token"}
	var runs []result
	for _, mode := range []string{"naive", "hardcap"} {
		r, err := stampede(t.Context(), client, mode, "0.05", loadgen.Config{
			URL: srv.URL, Model: "mock-1", Workers: 40, Stream: true,
			Duration: 30 * time.Second, StopAfterRefusals: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if r.stats.OK == 0 || r.stats.Refused == 0 || r.stats.Failed != 0 {
			t.Errorf("%s: %+v; want some served, some refused, none failed", mode, r.stats)
		}
		if r.audit.Project.Cap != 50_000 || !r.audit.Balanced || r.audit.Project.Reserved != 0 {
			t.Errorf("%s: audit %+v; want a 5-cent cap and balanced books with nothing held", mode, r.audit)
		}
		if r.audit.ReceiptCount != r.stats.OK {
			t.Errorf("%s: %d receipts for %d served requests", mode, r.audit.ReceiptCount, r.stats.OK)
		}
		runs = append(runs, r)
	}
	if hc := runs[1].audit.Project; hc.Spent > hc.Cap {
		t.Fatalf("CAP CROSSED in Hardcap mode: spent %d of %d", hc.Spent, hc.Cap)
	}

	var out strings.Builder
	report(&out, runs)
	for _, want := range []string{"over the cap", "never", "books balance"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report is missing %q:\n%s", want, out.String())
		}
	}
}

func TestAdminClientExplainsCommonMistakes(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	err := (&adminClient{url: srv.URL, token: "x"}).call("GET", "/admin/api/overview", "", nil)
	if err == nil || err.Error() != "the admin API is off: start Hardcap with HARDCAP_ADMIN_TOKEN set" {
		t.Errorf("admin API off: got %v", err)
	}
	err = (&adminClient{url: "http://127.0.0.1:1", token: "x"}).call("GET", "/admin/api/overview", "", nil)
	if err == nil {
		t.Error("an unreachable gateway should be an error")
	}
}

func TestCount(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 65046: "65,046", 1234567: "1,234,567"} {
		if got := count(n); got != want {
			t.Errorf("count(%d) = %q, want %q", n, got, want)
		}
	}
}
