// Package metrics is a small, dependency-free metrics registry that serves
// Prometheus's text format at /metrics.
//
// The official client library pulls in a dozen modules; the gateway needs
// three metric types and a text endpoint, which fit in this one file.
package metrics

import (
	"bufio"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds metrics and writes them out in registration order.
type Registry struct {
	mu      sync.Mutex
	metrics []writer
	names   map[string]bool
}

type writer interface {
	write(w *bufio.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{names: map[string]bool{}}
}

func (r *Registry) add(name string, m writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic("metrics: " + name + " registered twice")
	}
	r.names[name] = true
	r.metrics = append(r.metrics, m)
}

// ServeHTTP writes every metric in Prometheus's text exposition format.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriter(w)
	r.mu.Lock()
	all := slices.Clone(r.metrics)
	r.mu.Unlock()
	for _, m := range all {
		m.write(bw)
	}
	_ = bw.Flush()
}

// family is what all metric types share: a name, help text, label names,
// and one series per combination of label values.
type family[S any] struct {
	name, help, kind string
	labels           []string
	newSeries        func() *S

	mu     sync.Mutex
	series map[string]*seriesEntry[S]
}

type seriesEntry[S any] struct {
	values []string
	s      *S
}

func (f *family[S]) with(values []string) *S {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("metrics: %s takes %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\xff")
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.series[key]
	if !ok {
		e = &seriesEntry[S]{values: slices.Clone(values), s: f.newSeries()}
		f.series[key] = e
	}
	return e.s
}

// sorted returns the series in a stable order, so /metrics output is stable.
func (f *family[S]) sorted() []*seriesEntry[S] {
	f.mu.Lock()
	out := make([]*seriesEntry[S], 0, len(f.series))
	for _, e := range f.series {
		out = append(out, e)
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return slices.Compare(out[i].values, out[j].values) < 0
	})
	return out
}

func (f *family[S]) header(w *bufio.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
}

// labelString renders {a="x",b="y"}, plus any extra pair (a histogram's le).
func labelString(names, values []string, extra ...string) string {
	if len(names) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, n, escapeLabel(values[i]))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, extra[i], extra[i+1])
	}
	b.WriteByte('}')
	return b.String()
}

// atomicFloat is a float64 that's safe to update from many goroutines.
type atomicFloat struct{ bits atomic.Uint64 }

func (a *atomicFloat) add(v float64) {
	for {
		old := a.bits.Load()
		if a.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}
func (a *atomicFloat) set(v float64) { a.bits.Store(math.Float64bits(v)) }
func (a *atomicFloat) get() float64  { return math.Float64frombits(a.bits.Load()) }

// Counter only goes up: requests served, errors seen.
type Counter struct{ v atomicFloat }

func (c *Counter) Inc()           { c.v.add(1) }
func (c *Counter) Add(v float64)  { c.v.add(v) }
func (c *Counter) Value() float64 { return c.v.get() }

// CounterVec is a counter split by labels.
type CounterVec struct{ f *family[Counter] }

// Counter registers a counter. Pass label names to split it by labels.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	v := &CounterVec{&family[Counter]{name: name, help: help, kind: "counter", labels: labels,
		newSeries: func() *Counter { return &Counter{} }, series: map[string]*seriesEntry[Counter]{}}}
	r.add(name, v)
	return v
}

// With returns the counter for these label values, creating it if needed.
func (v *CounterVec) With(values ...string) *Counter { return v.f.with(values) }

func (v *CounterVec) write(w *bufio.Writer) {
	v.f.header(w)
	for _, e := range v.f.sorted() {
		fmt.Fprintf(w, "%s%s %s\n", v.f.name, labelString(v.f.labels, e.values), formatFloat(e.s.Value()))
	}
}

// Gauge goes up and down: requests in flight, a breaker's state.
type Gauge struct{ v atomicFloat }

func (g *Gauge) Set(v float64)  { g.v.set(v) }
func (g *Gauge) Add(v float64)  { g.v.add(v) }
func (g *Gauge) Value() float64 { return g.v.get() }

// GaugeVec is a gauge split by labels.
type GaugeVec struct{ f *family[Gauge] }

// Gauge registers a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	v := &GaugeVec{&family[Gauge]{name: name, help: help, kind: "gauge", labels: labels,
		newSeries: func() *Gauge { return &Gauge{} }, series: map[string]*seriesEntry[Gauge]{}}}
	r.add(name, v)
	return v
}

// With returns the gauge for these label values, creating it if needed.
func (v *GaugeVec) With(values ...string) *Gauge { return v.f.with(values) }

func (v *GaugeVec) write(w *bufio.Writer) {
	v.f.header(w)
	for _, e := range v.f.sorted() {
		fmt.Fprintf(w, "%s%s %s\n", v.f.name, labelString(v.f.labels, e.values), formatFloat(e.s.Value()))
	}
}

// Histogram counts observations (like durations) into buckets, so Prometheus
// can estimate percentiles such as p99.
type Histogram struct {
	bounds []float64       // upper bound of each bucket, ascending
	counts []atomic.Uint64 // per bucket, not cumulative; the last is +Inf
	sum    atomicFloat
	count  atomic.Uint64
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	h.counts[sort.SearchFloat64s(h.bounds, v)].Add(1)
	h.sum.add(v)
	h.count.Add(1)
}

// Count returns how many values were observed.
func (h *Histogram) Count() uint64 { return h.count.Load() }

// HistogramVec is a histogram split by labels.
type HistogramVec struct{ f *family[Histogram] }

// DefaultLatencyBuckets suit request latencies, in seconds: 1ms to 60s.
var DefaultLatencyBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// Histogram registers a histogram with the given bucket upper bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *HistogramVec {
	bounds := slices.Clone(buckets)
	slices.Sort(bounds)
	v := &HistogramVec{&family[Histogram]{name: name, help: help, kind: "histogram", labels: labels,
		newSeries: func() *Histogram {
			return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
		}, series: map[string]*seriesEntry[Histogram]{}}}
	r.add(name, v)
	return v
}

// With returns the histogram for these label values, creating it if needed.
func (v *HistogramVec) With(values ...string) *Histogram { return v.f.with(values) }

func (v *HistogramVec) write(w *bufio.Writer) {
	v.f.header(w)
	for _, e := range v.f.sorted() {
		h := e.s
		var cumulative uint64
		for i, bound := range h.bounds {
			cumulative += h.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket%s %d\n", v.f.name, labelString(v.f.labels, e.values, "le", formatFloat(bound)), cumulative)
		}
		cumulative += h.counts[len(h.bounds)].Load()
		fmt.Fprintf(w, "%s_bucket%s %d\n", v.f.name, labelString(v.f.labels, e.values, "le", "+Inf"), cumulative)
		fmt.Fprintf(w, "%s_sum%s %s\n", v.f.name, labelString(v.f.labels, e.values), formatFloat(h.sum.get()))
		fmt.Fprintf(w, "%s_count%s %d\n", v.f.name, labelString(v.f.labels, e.values), cumulative)
	}
}

// GaugeFunc registers a gauge whose value is read from fn at scrape time.
func (r *Registry) GaugeFunc(name, help string, fn func() float64) {
	r.add(name, &gaugeFunc{name: name, help: help, fn: fn})
}

type gaugeFunc struct {
	name, help string
	fn         func() float64
}

func (g *gaugeFunc) write(w *bufio.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", g.name, escapeHelp(g.help), g.name, g.name, formatFloat(g.fn()))
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
