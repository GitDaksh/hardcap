package admin_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/admin"
	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/gateway"
	"github.com/GitDaksh/hardcap/internal/ledger"
	"github.com/GitDaksh/hardcap/internal/mock"
	"github.com/GitDaksh/hardcap/internal/testenv"
)

const token = "test-admin-token"

type client struct {
	t   *testing.T
	url string
}

func (c client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// start runs the admin server (and, with demo on, a real gateway behind it
// for the stampede to hit).
func start(t *testing.T, demo bool) (client, *ledger.Ledger) {
	t.Helper()
	books := ledger.New(testenv.Postgres(t))
	var runner *admin.Demo
	if demo {
		upstream := httptest.NewServer(mock.NewHandler(mock.Config{TokenDelay: time.Millisecond, ReplyTokens: 5}))
		t.Cleanup(upstream.Close)
		models, _, err := config.Parse([]byte(fmt.Sprintf(`
providers: {mock: {base_url: "%s/v1"}}
models:
  mock-1:
    default_max_tokens: 5
    prompt_overhead_tokens: 0
    routes: [{provider: mock, input_per_million: 10, output_per_million: 30}]
`, upstream.URL)), func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		gw, err := gateway.New(gateway.Config{Models: models, Ledger: books})
		if err != nil {
			t.Fatal(err)
		}
		gwSrv := httptest.NewServer(gw.Handler())
		t.Cleanup(gwSrv.Close)
		runner = admin.NewDemo(gwSrv.URL, books, nil)
	}
	srv, err := admin.New(admin.Config{Ledger: books, Token: token, Models: []string{"mock-1"}, Demo: runner})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return client{t, s.URL}, books
}

func TestNeedsTheToken(t *testing.T) {
	c, _ := start(t, false)
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x"} {
		req, _ := http.NewRequest(http.MethodGet, c.url+"/admin/api/projects", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
	if _, err := admin.New(admin.Config{Token: "short"}); err == nil {
		t.Error("a short admin token should be refused")
	}
}

func TestProjectsAndKeys(t *testing.T) {
	c, books := start(t, false)

	status, p := c.do("POST", "/admin/api/projects", `{"name":"chatbot","cap_usd":2.5}`)
	if status != 201 || p["cap_micros"] != float64(2_500_000) || p["mode"] != "hardcap" {
		t.Fatalf("create: %d %v", status, p)
	}
	id := int64(p["id"].(float64))
	path := fmt.Sprintf("/admin/api/projects/%d", id)

	if status, _ := c.do("POST", "/admin/api/projects", `{"name":"chatbot","cap_usd":1}`); status != 409 {
		t.Errorf("duplicate name: %d, want 409", status)
	}
	for _, bad := range []string{`{"name":"","cap_usd":1}`, `{"name":"x","cap_usd":-1}`, `{"name":"x","cap_usd":1,"mode":"yolo"}`, `{"name":"x","cap":1}`, `nope`} {
		if status, _ := c.do("POST", "/admin/api/projects", bad); status != 400 {
			t.Errorf("%s: %d, want 400", bad, status)
		}
	}

	if status, p := c.do("PATCH", path, `{"cap_usd":"0.75","mode":"naive"}`); status != 200 || p["cap_micros"] != float64(750_000) || p["mode"] != "naive" {
		t.Errorf("update: %d %v", status, p)
	}

	status, k := c.do("POST", path+"/keys", `{"name":"prod","cap_usd":0.5,"rpm_limit":60}`)
	secret, _ := k["secret"].(string)
	if status != 201 || !strings.HasPrefix(secret, "hc_") {
		t.Fatalf("create key: %d %v", status, k)
	}
	key, err := books.Authenticate(t.Context(), secret)
	if err != nil || *key.Cap != 500_000 || *key.RPM != 60 {
		t.Fatalf("the new key doesn't work: %+v %v", key, err)
	}
	if status, _ := c.do("POST", fmt.Sprintf("/admin/api/keys/%d/revoke", key.ID), ""); status != 204 {
		t.Errorf("revoke: %d", status)
	}
	if status, _ := c.do("POST", "/admin/api/projects/999/keys", `{"name":"x"}`); status != 404 {
		t.Errorf("key for a missing project: %d, want 404", status)
	}

	status, a := c.do("GET", path+"/audit", "")
	if status != 200 || a["balanced"] != true || a["within_cap"] != true {
		t.Errorf("audit: %d %v", status, a)
	}
	status, o := c.do("GET", "/admin/api/overview", "")
	if projects, _ := o["projects"].([]any); status != 200 || len(projects) != 1 || o["demo"] != false {
		t.Errorf("overview: %d %v", status, o)
	}
	if status, _ := c.do("POST", path+"/stampede", `{"workers":1,"seconds":1}`); status != 404 {
		t.Errorf("stampede with demo mode off: %d, want 404", status)
	}
	if status, _ := c.do("GET", "/admin/api/projects/abc", ""); status != 400 {
		t.Errorf("bad id: %d, want 400", status)
	}
}

func TestDashboardIsServed(t *testing.T) {
	c, _ := start(t, false)
	for path, want := range map[string]string{"/dashboard/": "<title>Hardcap</title>", "/dashboard/app.js": "CAP_AT", "/dashboard/style.css": "--cap-at"} {
		resp, err := http.Get(c.url + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Get(c.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/dashboard/" {
		t.Errorf("/ should redirect to the dashboard, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// The dashboard's Stampede button, end to end: 50 clients for 2 seconds
// against a 5-cent cap, through a real gateway.
func TestStampedeHoldsTheCap(t *testing.T) {
	c, books := start(t, true)
	_, p := c.do("POST", "/admin/api/projects", `{"name":"demo","cap_usd":0.05}`)
	id := int64(p["id"].(float64))
	path := fmt.Sprintf("/admin/api/projects/%d", id)

	if status, body := c.do("POST", path+"/stampede", `{"workers":50,"seconds":2,"model":"mock-1"}`); status != 202 {
		t.Fatalf("start: %d %v", status, body)
	}
	if status, _ := c.do("POST", path+"/stampede", `{"workers":50,"seconds":2}`); status != 409 {
		t.Errorf("a second stampede at once: %d, want 409", status)
	}
	if status, _ := c.do("POST", path+"/reset", ""); status != 409 {
		t.Errorf("reset during a stampede: %d, want 409", status)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, view := c.do("GET", path, "")
		s, _ := view["stampede"].(map[string]any)
		if s != nil && s["running"] == false {
			if s["ok"].(float64) == 0 || s["refused"].(float64) == 0 || s["failed"].(float64) != 0 {
				t.Errorf("stampede result: %v; want some served, some refused, none failed", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stampede never finished")
		}
		time.Sleep(100 * time.Millisecond)
	}

	a, err := books.Audit(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Project.Spent > a.Project.Cap || a.Project.Reserved != 0 || !a.Balanced() {
		t.Fatalf("after the stampede: %+v", a)
	}
	keys, _ := books.Keys(t.Context(), id)
	if len(keys) != 1 || !keys[0].Revoked {
		t.Errorf("the stampede's throwaway key should be revoked: %+v", keys)
	}
}
