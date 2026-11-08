package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GitDaksh/hardcap/internal/config"
	"github.com/GitDaksh/hardcap/internal/pricing"
)

const sample = `
limits:
  default_rpm: 600
  default_tpm: 100000
providers:
  mock:
    base_url: http://localhost:8090/v1/
  groq:
    base_url: https://api.groq.com/openai/v1
    api_key_env: TEST_GROQ_KEY
    max_tokens_field: max_completion_tokens
models:
  fast:
    default_max_tokens: 100
    prompt_overhead_tokens: 0
    routes:
      - provider: groq
        model: openai/gpt-oss-20b
        input_per_million: 0.075
        output_per_million: 0.30
      - provider: mock
        input_per_million: 1
        output_per_million: 0.10
  groq-only:
    routes:
      - provider: groq
        model: openai/gpt-oss-120b
`

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestParseWithEveryProviderOn(t *testing.T) {
	cfg, warnings, err := config.Parse([]byte(sample), env(map[string]string{"TEST_GROQ_KEY": "gsk_test"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none", warnings)
	}

	fast := cfg.Models["fast"]
	if fast == nil || len(fast.Routes) != 2 {
		t.Fatalf("fast = %+v, want 2 routes", fast)
	}
	if fast.Routes[1].Model != "fast" {
		t.Errorf("a route without a model name should default to the model's own name, got %q", fast.Routes[1].Model)
	}
	if fast.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens = %d, want the 4096 default", fast.MaxOutputTokens)
	}
	if fast.PromptOverheadTokens != 0 || cfg.Models["groq-only"].PromptOverheadTokens != 256 {
		t.Errorf("prompt overhead = %d and %d, want 0 (explicit) and 256 (default)",
			fast.PromptOverheadTokens, cfg.Models["groq-only"].PromptOverheadTokens)
	}
	// Worst case across routes: groq's output price, the mock's input price.
	want := pricing.Price{InputPerMillion: 1_000_000, OutputPerMillion: 300_000}
	if got := fast.WorstPrice(); got != want {
		t.Errorf("WorstPrice = %+v, want %+v", got, want)
	}

	if g := cfg.Providers["groq"]; g.APIKey != "gsk_test" || g.MaxTokensField != "max_completion_tokens" {
		t.Errorf("groq = %+v", g)
	}
	if m := cfg.Providers["mock"]; m.BaseURL != "http://localhost:8090/v1" || m.MaxTokensField != "max_tokens" {
		t.Errorf("mock = %+v (trailing slash trimmed, default field)", m)
	}
	if cfg.Routing.AttemptsPerRoute != 2 || cfg.Routing.BreakerFailures != 5 || cfg.Routing.BreakerCooldown != 30*time.Second {
		t.Errorf("routing defaults = %+v", cfg.Routing)
	}
	if got := strings.Join(cfg.ModelNames(), ","); got != "fast,groq-only" {
		t.Errorf("ModelNames = %s", got)
	}
}

func TestMissingAPIKeySwitchesProviderOff(t *testing.T) {
	cfg, warnings, err := config.Parse([]byte(sample), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Providers["groq"]; ok {
		t.Error("groq should be switched off without its key")
	}
	if fast := cfg.Models["fast"]; len(fast.Routes) != 1 || fast.Routes[0].Provider != "mock" {
		t.Errorf("fast should keep only its mock route, got %+v", fast.Routes)
	}
	if _, ok := cfg.Models["groq-only"]; ok {
		t.Error("groq-only has no providers left and should be switched off")
	}
	// The cause (a provider is off) comes before its effect (a model is off).
	if len(warnings) != 2 || !strings.HasPrefix(warnings[0], `provider "groq"`) || !strings.HasPrefix(warnings[1], `model "groq-only"`) {
		t.Errorf("warnings = %q, want groq's then groq-only's", warnings)
	}
}

func TestRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"typo in a field": `
providers: {mock: {base_url: "http://x/v1", bse_url: "oops"}}
models: {m: {routes: [{provider: mock}]}}`,
		"unknown provider": `
providers: {mock: {base_url: "http://x/v1"}}
models: {m: {routes: [{provider: nope}]}}`,
		"bad url": `
providers: {mock: {base_url: "localhost:8090"}}
models: {m: {routes: [{provider: mock}]}}`,
		"negative price": `
providers: {mock: {base_url: "http://x/v1"}}
models: {m: {routes: [{provider: mock, input_per_million: -1}]}}`,
		"default above max": `
providers: {mock: {base_url: "http://x/v1"}}
models: {m: {default_max_tokens: 500, max_output_tokens: 100, routes: [{provider: mock}]}}`,
		"no routes": `
providers: {mock: {base_url: "http://x/v1"}}
models: {m: {routes: []}}`,
		"negative overhead": `
providers: {mock: {base_url: "http://x/v1"}}
models: {m: {prompt_overhead_tokens: -1, routes: [{provider: mock}]}}`,
		"bad tokens field": `
providers: {mock: {base_url: "http://x/v1", max_tokens_field: tokens}}
models: {m: {routes: [{provider: mock}]}}`,
		"empty": ``,
	}
	for name, yml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := config.Parse([]byte(yml), env(nil)); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// The real hardcap.yaml in the repo must always load.
func TestRepoConfigLoads(t *testing.T) {
	cfg, _, err := config.Load("../../hardcap.yaml", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Models["mock-1"]; !ok {
		t.Error("hardcap.yaml should always include the free mock-1 model")
	}
}
