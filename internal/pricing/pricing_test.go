package pricing_test

import (
	"testing"

	"github.com/GitDaksh/hardcap/internal/pricing"
)

func TestParseUSD(t *testing.T) {
	good := map[string]pricing.Micros{
		"1":         1_000_000,
		"0.25":      250_000,
		"$0.075":    75_000,
		".5":        500_000,
		"12.000001": 12_000_001,
		"0":         0,
		" 3.10 ":    3_100_000,
	}
	for in, want := range good {
		got, err := pricing.ParseUSD(in)
		if err != nil || got != want {
			t.Errorf("ParseUSD(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".", "abc", "-1", "+1", "1.0000001", "1.2.3", "1e3", "0.-5"} {
		if got, err := pricing.ParseUSD(in); err == nil {
			t.Errorf("ParseUSD(%q) = %d, want an error", in, got)
		}
	}
}

func TestFromUSD(t *testing.T) {
	// 0.07 can't be stored exactly as a float (it's 0.07000000000000000666...),
	// so converting must round, not truncate.
	if got, err := pricing.FromUSD(0.07); err != nil || got != 70_000 {
		t.Errorf("FromUSD(0.07) = %d, %v; want 70000", got, err)
	}
	if _, err := pricing.FromUSD(-1); err == nil {
		t.Error("FromUSD(-1) should fail")
	}
}

func TestString(t *testing.T) {
	cases := map[pricing.Micros]string{
		1_500_000: "$1.500000",
		42:        "$0.000042",
		-7:        "-$0.000007",
	}
	for in, want := range cases {
		if got := in.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(in), got, want)
		}
	}
}

func TestCostRoundsUp(t *testing.T) {
	// $0.075 per million input tokens, $0.30 per million output tokens.
	p := pricing.Price{InputPerMillion: 75_000, OutputPerMillion: 300_000}

	// 1,000 input tokens cost exactly $0.000075 = 75 micros.
	if got := p.Cost(1_000, 0); got != 75 {
		t.Errorf("Cost(1000, 0) = %d, want 75", got)
	}
	// One output token costs 0.3 micros: we round up to 1, never down to 0.
	if got := p.Cost(0, 1); got != 1 {
		t.Errorf("Cost(0, 1) = %d, want 1 (rounded up)", got)
	}
	if got := p.Cost(0, 0); got != 0 {
		t.Errorf("Cost(0, 0) = %d, want 0", got)
	}
	// A million of each: $0.075 + $0.30 = $0.375.
	if got := p.Cost(1_000_000, 1_000_000); got != 375_000 {
		t.Errorf("Cost(1M, 1M) = %d, want 375000", got)
	}
}

func TestMax(t *testing.T) {
	a := pricing.Price{InputPerMillion: 10, OutputPerMillion: 50}
	b := pricing.Price{InputPerMillion: 30, OutputPerMillion: 20}
	if got := pricing.Max(a, b); got != (pricing.Price{InputPerMillion: 30, OutputPerMillion: 50}) {
		t.Errorf("Max = %+v", got)
	}
}
