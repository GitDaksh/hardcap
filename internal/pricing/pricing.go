// Package pricing does Hardcap's money math.
//
// All money is stored as whole micro-dollars (millionths of a US dollar) in
// an int64, never as a float: floats can't represent most decimal amounts
// exactly, and tiny rounding errors add up over millions of requests.
package pricing

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Micros is an amount of money in millionths of a US dollar: $1 = 1_000_000.
type Micros int64

// PerDollar is how many Micros make one dollar.
const PerDollar Micros = 1_000_000

// ParseUSD reads a dollar amount like "1", "0.25" or "$0.075" exactly, with
// up to 6 decimal places. It never goes through a float.
func ParseUSD(s string) (Micros, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "$")
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, fmt.Errorf("%q is not a dollar amount", s)
	}
	if strings.HasPrefix(whole, "-") || strings.HasPrefix(whole, "+") {
		return 0, fmt.Errorf("%q: amounts must be positive digits, like 1.25", s)
	}
	if hasDot && len(frac) > 6 {
		return 0, fmt.Errorf("%q has more than 6 decimal places", s)
	}
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a dollar amount", s)
	}
	if w > math.MaxInt64/int64(PerDollar)-1 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	var f int64
	if frac != "" {
		f, err = strconv.ParseInt(frac+strings.Repeat("0", 6-len(frac)), 10, 64)
		if err != nil || strings.ContainsAny(frac, "+-") {
			return 0, fmt.Errorf("%q is not a dollar amount", s)
		}
	}
	return Micros(w)*PerDollar + Micros(f), nil
}

// FromUSD converts a float dollar amount (e.g. from JSON) to Micros,
// rounding to the nearest micro-dollar.
func FromUSD(usd float64) (Micros, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return 0, errors.New("amount must be a positive number")
	}
	if usd > float64(math.MaxInt64/int64(PerDollar)) {
		return 0, errors.New("amount is too large")
	}
	return Micros(math.Round(usd * float64(PerDollar))), nil
}

// USD returns the amount in dollars, for display and JSON only.
func (m Micros) USD() float64 {
	return float64(m) / float64(PerDollar)
}

// String formats the amount like "$1.234567".
func (m Micros) String() string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	return fmt.Sprintf("%s$%d.%06d", sign, m/PerDollar, m%PerDollar)
}

// Price is what one model costs, in Micros per million tokens. A model that
// costs $0.075 per million input tokens has InputPerMillion = 75_000.
type Price struct {
	InputPerMillion  Micros
	OutputPerMillion Micros
}

// Cost returns the price of the given token counts, rounded UP to the next
// whole micro-dollar, so Hardcap never charges less than the real cost.
func (p Price) Cost(promptTokens, completionTokens int64) Micros {
	total := promptTokens*int64(p.InputPerMillion) + completionTokens*int64(p.OutputPerMillion)
	if total <= 0 {
		return 0
	}
	return Micros((total + 999_999) / 1_000_000)
}

// Max returns a price that is at least as expensive as both a and b for any
// mix of tokens. Hardcap holds the worst case, so when a model can be served
// by several providers, it holds what the most expensive one would charge.
func Max(a, b Price) Price {
	return Price{
		InputPerMillion:  max(a.InputPerMillion, b.InputPerMillion),
		OutputPerMillion: max(a.OutputPerMillion, b.OutputPerMillion),
	}
}
