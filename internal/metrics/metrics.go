package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Registry owns the gateway's Prometheus metrics. It wraps a private
// *prometheus.Registry so the default process collector and Go
// runtime collector are not registered: the gateway's published
// metric surface is a stable contract, and picking up go_gc_*
// implicitly would silently expand that surface.
//
// A single Registry is constructed at wire-up time in cmd/gateway and
// handed to every collaborator that needs to increment a metric. All
// operations on a Registry are safe for concurrent use because
// client_golang's primitives are.
type Registry struct {
	r *prometheus.Registry

	requests        *CounterVec
	requestDuration *HistogramVec
	inFlight        *Gauge
	toolCalls       *CounterVec
	toolDuration    *HistogramVec
	modelTokens     *CounterVec
	upstreamErrors  *CounterVec
	rateLimited     *Counter
}

// defaultBuckets mirrors prometheus/client_golang's default histogram
// buckets — the range that covers typical web-request latencies. Tool
// calls and chat responses both fit comfortably inside this range.
// Kept explicit rather than reusing prometheus.DefBuckets so the
// contract this package publishes does not silently follow upstream
// default changes.
var defaultBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// NewRegistry returns a Registry with every documented metric
// pre-registered. Pre-registration means /metrics emits the full
// schema even before any request has been served, which keeps
// Prometheus's `absent_over_time` checks sensible.
func NewRegistry() *Registry {
	r := prometheus.NewRegistry()

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opencost_ai_gateway_requests_total",
			Help: "Total HTTP requests processed by the gateway, partitioned by endpoint, method, and status code.",
		},
		[]string{"endpoint", "method", "status"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opencost_ai_gateway_request_duration_seconds",
			Help:    "Wall-clock duration of HTTP requests handled by the gateway.",
			Buckets: defaultBuckets,
		},
		[]string{"endpoint", "method"},
	)
	inFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "opencost_ai_gateway_requests_in_flight",
		Help: "Number of HTTP requests currently being handled by the gateway.",
	})
	toolCalls := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opencost_ai_gateway_tool_calls_total",
			Help: "Total MCP tool invocations observed while answering chat requests.",
		},
		[]string{"tool"},
	)
	toolDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "opencost_ai_gateway_tool_call_duration_seconds",
			Help:    "Wall-clock duration of individual MCP tool invocations.",
			Buckets: defaultBuckets,
		},
		[]string{"tool"},
	)
	modelTokens := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opencost_ai_gateway_model_tokens_total",
			Help: "Prompt and completion token totals reported by the bridge, per model.",
		},
		[]string{"model", "kind"},
	)
	upstreamErrors := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "opencost_ai_gateway_upstream_errors_total",
			Help: "Upstream bridge errors by operation and failure kind (transport, http_4xx, http_5xx).",
		},
		[]string{"op", "kind"},
	)
	rateLimited := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "opencost_ai_gateway_rate_limited_total",
		Help: "Requests rejected by the per-caller rate limiter.",
	})

	r.MustRegister(
		requests, requestDuration, inFlight,
		toolCalls, toolDuration,
		modelTokens, upstreamErrors, rateLimited,
	)

	return &Registry{
		r:               r,
		requests:        &CounterVec{cv: requests},
		requestDuration: &HistogramVec{hv: requestDuration},
		inFlight:        &Gauge{g: inFlight},
		toolCalls:       &CounterVec{cv: toolCalls},
		toolDuration:    &HistogramVec{hv: toolDuration},
		modelTokens:     &CounterVec{cv: modelTokens},
		upstreamErrors:  &CounterVec{cv: upstreamErrors},
		rateLimited:     &Counter{c: rateLimited},
	}
}

// Requests returns the counter for total HTTP requests.
func (r *Registry) Requests() *CounterVec { return r.requests }

// RequestDuration returns the histogram for request wall-clock time.
func (r *Registry) RequestDuration() *HistogramVec { return r.requestDuration }

// InFlight returns the gauge for in-flight requests.
func (r *Registry) InFlight() *Gauge { return r.inFlight }

// ToolCalls returns the counter for per-tool invocation counts.
func (r *Registry) ToolCalls() *CounterVec { return r.toolCalls }

// ToolDuration returns the histogram for per-tool wall-clock time.
func (r *Registry) ToolDuration() *HistogramVec { return r.toolDuration }

// ModelTokens returns the counter for per-model token totals.
func (r *Registry) ModelTokens() *CounterVec { return r.modelTokens }

// UpstreamErrors returns the counter for upstream bridge failures.
func (r *Registry) UpstreamErrors() *CounterVec { return r.upstreamErrors }

// RateLimited returns the counter for rate-limited requests.
func (r *Registry) RateLimited() *Counter { return r.rateLimited }

// Handler returns an http.Handler serving Prometheus text exposition
// for every registered family. Content-Type follows the Prometheus
// 0.0.4 text format; OpenMetrics negotiation is intentionally left
// disabled so the gateway's metric surface stays on the format its
// contract already documents.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.r, promhttp.HandlerOpts{})
}

// ---- wrappers ------------------------------------------------------
//
// The wrappers below exist so collaborator packages (internal/server,
// cmd/gateway) depend on this package's narrow surface rather than
// the full client_golang API. Swapping the backing implementation in
// the future — native histograms, multi-registry, OpenTelemetry
// bridge — is then a contained change.

// Counter is a monotonically increasing float64 metric with no labels.
type Counter struct{ c prometheus.Counter }

// Inc increments the counter by 1.
func (c *Counter) Inc() { c.c.Inc() }

// Add increments the counter by delta. client_golang panics on
// negative delta; the old hand-rolled implementation silently ignored
// them, but nothing in the gateway actually passes a negative value —
// preserving the panic makes that assumption explicit and catches
// regressions rather than hiding them.
func (c *Counter) Add(delta float64) { c.c.Add(delta) }

// Value returns the current counter value. Exposed for tests that
// assert increments from the outside; production code never inspects
// metric values directly.
func (c *Counter) Value() float64 { return metricFloat(c.c) }

// CounterVec is a counter family partitioned by a fixed label set.
type CounterVec struct{ cv *prometheus.CounterVec }

// WithLabelValues returns a handle scoped to the supplied label
// values. Panics if len(values) does not match the registered label
// names — this is a programming error, not a runtime condition, and
// matches client_golang's contract.
func (v *CounterVec) WithLabelValues(values ...string) *Counter {
	return &Counter{c: v.cv.WithLabelValues(values...)}
}

// Gauge is a float64 value that can move in either direction.
type Gauge struct{ g prometheus.Gauge }

// Inc bumps the gauge by 1.
func (g *Gauge) Inc() { g.g.Inc() }

// Dec subtracts 1 from the gauge.
func (g *Gauge) Dec() { g.g.Dec() }

// Add adjusts the gauge by delta (may be negative).
func (g *Gauge) Add(delta float64) { g.g.Add(delta) }

// Set overwrites the gauge.
func (g *Gauge) Set(v float64) { g.g.Set(v) }

// Value returns the current gauge value. Exposed for tests.
func (g *Gauge) Value() float64 { return metricFloat(g.g) }

// Histogram is an observer of sample values bucketed per the
// histogram's configured boundaries.
type Histogram struct{ o prometheus.Observer }

// Observe records a single sample in seconds (or whatever unit the
// histogram's name advertises).
func (h *Histogram) Observe(v float64) { h.o.Observe(v) }

// HistogramVec is a histogram family partitioned by a fixed label
// set. Bucket bounds are shared across all series.
type HistogramVec struct{ hv *prometheus.HistogramVec }

// WithLabelValues returns a handle scoped to values.
func (v *HistogramVec) WithLabelValues(values ...string) *Histogram {
	return &Histogram{o: v.hv.WithLabelValues(values...)}
}

// metricFloat reads a single-sample Counter or Gauge's current value
// via the dto round-trip that client_golang already uses internally.
// It is defensive by design: unrecognised metric shapes return 0
// rather than panicking, because a nil dereference here during a
// hot-path Value() call would take the gateway down for a test-only
// observation.
func metricFloat(m prometheus.Metric) float64 {
	var pb dto.Metric
	if err := m.Write(&pb); err != nil {
		return 0
	}
	switch {
	case pb.Counter != nil:
		return pb.Counter.GetValue()
	case pb.Gauge != nil:
		return pb.Gauge.GetValue()
	case pb.Untyped != nil:
		return pb.Untyped.GetValue()
	}
	return 0
}
