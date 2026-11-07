// Package config loads hardcap.yaml: which AI providers exist, which models
// clients can ask for, which provider serves each model (in fallback order),
// and what each one costs.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/GitDaksh/hardcap/internal/pricing"
)

// Config is the whole hardcap.yaml file.
type Config struct {
	Limits    Limits               `yaml:"limits"`
	Routing   Routing              `yaml:"routing"`
	Providers map[string]*Provider `yaml:"providers"`
	Models    map[string]*Model    `yaml:"models"`
}

// Limits are the default rate limits for API keys that don't set their own.
// Zero means "no limit".
type Limits struct {
	DefaultRPM int `yaml:"default_rpm"` // requests per minute
	DefaultTPM int `yaml:"default_tpm"` // tokens per minute
}

// Routing controls retries and circuit breakers.
type Routing struct {
	// AttemptsPerRoute is how many times one provider is tried before
	// falling back to the next one.
	AttemptsPerRoute int `yaml:"attempts_per_route"`
	// BreakerFailures is how many failures in a row open a provider's
	// circuit breaker.
	BreakerFailures int `yaml:"breaker_failures"`
	// BreakerCooldown is how long an open breaker skips its provider
	// before letting one trial request through.
	BreakerCooldown time.Duration `yaml:"breaker_cooldown"`
}

// Provider is one OpenAI-compatible API, like Groq, Ollama or the mock.
type Provider struct {
	Name    string `yaml:"-"`
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable that holds the API key, so
	// the key itself never lives in this file (or in git).
	APIKeyEnv string `yaml:"api_key_env"`
	APIKey    string `yaml:"-"`
	// MaxTokensField is the request field this provider reads the output
	// limit from: "max_tokens" (the default) or "max_completion_tokens".
	MaxTokensField string `yaml:"max_tokens_field"`
}

// Model is a model name that clients can ask for.
type Model struct {
	Name string `yaml:"-"`
	// DefaultMaxTokens is used when a request doesn't set max_tokens. Every
	// request needs a limit: you can't cap the cost of an unbounded reply.
	DefaultMaxTokens int `yaml:"default_max_tokens"`
	// MaxOutputTokens is the largest max_tokens a request may ask for.
	MaxOutputTokens int `yaml:"max_output_tokens"`
	// PromptOverheadTokens is how many tokens a provider may add around the
	// messages (chat template, built-in system prompt). Hardcap adds it to
	// the request's size when it bounds the prompt. Default 256.
	PromptOverheadTokens int  `yaml:"-"`
	RawPromptOverhead    *int `yaml:"prompt_overhead_tokens"`
	// Routes are the providers that can serve this model, in the order
	// they're tried.
	Routes []Route `yaml:"routes"`
}

// Route is one way to serve a model: a provider, the model's name at that
// provider, and that provider's price.
type Route struct {
	Provider         string `yaml:"provider"`
	Model            string `yaml:"model"`
	InputPerMillion  USD    `yaml:"input_per_million"`
	OutputPerMillion USD    `yaml:"output_per_million"`
}

// Price returns what this route charges per million tokens.
func (r Route) Price() pricing.Price {
	return pricing.Price{InputPerMillion: r.InputPerMillion.Micros, OutputPerMillion: r.OutputPerMillion.Micros}
}

// WorstPrice is the most this model can cost per token on any of its routes.
func (m *Model) WorstPrice() pricing.Price {
	var p pricing.Price
	for _, r := range m.Routes {
		p = pricing.Max(p, r.Price())
	}
	return p
}

// USD is a dollar amount in YAML, like 0.075. It's read as exact text, never
// as a float.
type USD struct {
	Micros pricing.Micros
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (u *USD) UnmarshalYAML(node *yaml.Node) error {
	m, err := pricing.ParseUSD(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	u.Micros = m
	return nil
}

// Defaults for settings that hardcap.yaml leaves out.
const (
	defaultAttemptsPerRoute = 2
	defaultBreakerFailures  = 5
	defaultBreakerCooldown  = 30 * time.Second
	defaultMaxTokens        = 256
	defaultMaxOutputTokens  = 4096
	defaultPromptOverhead   = 256
)

// Load reads and checks the config file at path. getenv looks up provider
// API keys (pass os.Getenv).
//
// A provider whose API key isn't set is switched off instead of failing, so
// you can run Hardcap with just the mock and add real providers later. Each
// one switched off produces a warning.
func Load(path string, getenv func(string) string) (*Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return Parse(data, getenv)
}

// Parse is Load for config that's already in memory.
func Parse(data []byte, getenv func(string) string) (*Config, []string, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a typo in a field name is an error, not a silently ignored setting
	if err := dec.Decode(&cfg); err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}

	var warnings []string
	if err := cfg.applyDefaults(); err != nil {
		return nil, nil, err
	}

	disabled := map[string]bool{}
	for name, p := range cfg.Providers {
		if p == nil {
			return nil, nil, fmt.Errorf("config: provider %q is empty", name)
		}
		p.Name = name
		if err := p.check(); err != nil {
			return nil, nil, err
		}
		if p.APIKeyEnv != "" {
			p.APIKey = getenv(p.APIKeyEnv)
			if p.APIKey == "" {
				disabled[name] = true
				warnings = append(warnings, fmt.Sprintf("provider %q is off: set %s to use it", name, p.APIKeyEnv))
			}
		}
	}

	for name, m := range cfg.Models {
		if m == nil {
			return nil, nil, fmt.Errorf("config: model %q is empty", name)
		}
		m.Name = name
		if err := m.check(cfg.Providers); err != nil {
			return nil, nil, err
		}
		usable := m.Routes[:0]
		for _, r := range m.Routes {
			if !disabled[r.Provider] {
				usable = append(usable, r)
			}
		}
		m.Routes = usable
		if len(m.Routes) == 0 {
			delete(cfg.Models, name)
			warnings = append(warnings, fmt.Sprintf("model %q is off: none of its providers are on", name))
		}
	}
	if len(cfg.Models) == 0 {
		return nil, warnings, errors.New("config: no usable models (check the warnings above)")
	}
	for name := range disabled {
		delete(cfg.Providers, name)
	}
	slices.Sort(warnings)
	return &cfg, warnings, nil
}

// ModelNames returns the usable model names, sorted.
func (c *Config) ModelNames() []string {
	names := make([]string, 0, len(c.Models))
	for name := range c.Models {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (c *Config) applyDefaults() error {
	if c.Limits.DefaultRPM < 0 || c.Limits.DefaultTPM < 0 {
		return errors.New("config: limits can't be negative (use 0 for no limit)")
	}
	r := &c.Routing
	if r.AttemptsPerRoute == 0 {
		r.AttemptsPerRoute = defaultAttemptsPerRoute
	}
	if r.BreakerFailures == 0 {
		r.BreakerFailures = defaultBreakerFailures
	}
	if r.BreakerCooldown == 0 {
		r.BreakerCooldown = defaultBreakerCooldown
	}
	if r.AttemptsPerRoute < 1 || r.AttemptsPerRoute > 5 {
		return errors.New("config: routing.attempts_per_route must be between 1 and 5")
	}
	if r.BreakerFailures < 1 || r.BreakerCooldown < 0 {
		return errors.New("config: routing.breaker_failures must be at least 1 and breaker_cooldown positive")
	}
	if len(c.Providers) == 0 || len(c.Models) == 0 {
		return errors.New("config: needs at least one provider and one model")
	}
	return nil
}

func (p *Provider) check() error {
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("config: provider %q: base_url %q should look like http://localhost:8090/v1", p.Name, p.BaseURL)
	}
	p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	switch p.MaxTokensField {
	case "":
		p.MaxTokensField = "max_tokens"
	case "max_tokens", "max_completion_tokens":
	default:
		return fmt.Errorf("config: provider %q: max_tokens_field must be max_tokens or max_completion_tokens", p.Name)
	}
	return nil
}

func (m *Model) check(providers map[string]*Provider) error {
	if m.DefaultMaxTokens == 0 {
		m.DefaultMaxTokens = defaultMaxTokens
	}
	if m.MaxOutputTokens == 0 {
		m.MaxOutputTokens = max(defaultMaxOutputTokens, m.DefaultMaxTokens)
	}
	if m.DefaultMaxTokens < 1 || m.MaxOutputTokens < m.DefaultMaxTokens {
		return fmt.Errorf("config: model %q: need 1 <= default_max_tokens <= max_output_tokens", m.Name)
	}
	m.PromptOverheadTokens = defaultPromptOverhead
	if m.RawPromptOverhead != nil {
		m.PromptOverheadTokens = *m.RawPromptOverhead
	}
	if m.PromptOverheadTokens < 0 {
		return fmt.Errorf("config: model %q: prompt_overhead_tokens can't be negative", m.Name)
	}
	if len(m.Routes) == 0 {
		return fmt.Errorf("config: model %q has no routes", m.Name)
	}
	for i := range m.Routes {
		r := &m.Routes[i]
		if _, ok := providers[r.Provider]; !ok {
			return fmt.Errorf("config: model %q: route %d uses unknown provider %q", m.Name, i+1, r.Provider)
		}
		if r.Model == "" {
			r.Model = m.Name
		}
	}
	return nil
}
