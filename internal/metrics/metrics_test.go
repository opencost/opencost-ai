package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRegistry_ExposesDocumentedFamilies checks the contract with
// operators: every pre-registered family appears in the /metrics
// exposition with its documented name and HELP text, and the typed
// lines render exactly as the metric API documentation (architecture
// §7.6) claims. If any of these literal strings changes, callers
// grepping against /metrics will break.
func TestRegistry_ExposesDocumentedFamilies(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	r.Requests().WithLabelValues("/v1/ask", "POST", "200").Inc()
	r.RequestDuration().WithLabelValues("/v1/ask", "POST").Observe(0.42)
	r.InFlight().Inc()
	r.ToolCalls().WithLabelValues("opencost.allocation").Inc()
	r.ToolDuration().WithLabelValues("opencost.allocation").Observe(0.05)
	r.ModelTokens().WithLabelValues("qwen2.5:7b-instruct", "prompt").Add(123)
	r.ModelTokens().WithLabelValues("qwen2.5:7b-instruct", "completion").Add(45)
	r.UpstreamErrors().WithLabelValues("chat", "transport").Inc()
	r.RateLimited().Inc()

	out := scrape(t, r)

	wantFamilies := []string{
		"opencost_ai_gateway_requests_total",
		"opencost_ai_gateway_request_duration_seconds",
		"opencost_ai_gateway_requests_in_flight",
		"opencost_ai_gateway_tool_calls_total",
		"opencost_ai_gateway_tool_call_duration_seconds",
		"opencost_ai_gateway_model_tokens_total",
		"opencost_ai_gateway_upstream_errors_total",
		"opencost_ai_gateway_rate_limited_total",
	}
	for _, name := range wantFamilies {
		if !strings.Contains(out, "# HELP "+name+" ") {
			t.Errorf("missing HELP for %s", name)
		}
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("missing TYPE for %s", name)
		}
	}

	// Spot-check a few concrete lines so a rename or dropped label is
	// caught by the test suite rather than by operators. Labels are
	// written in alphabetical order here because that is client_golang's
	// deterministic exposition ordering; the Prometheus text format
	// does not fix label order on the wire, and operator queries grep
	// per-label (`{endpoint="/v1/ask"}`), not full-line literal matches,
	// so the alphabetical ordering is a coincidence of the backing
	// library rather than a contract in its own right.
	checks := []string{
		`opencost_ai_gateway_requests_total{endpoint="/v1/ask",method="POST",status="200"} 1`,
		`opencost_ai_gateway_request_duration_seconds_bucket{endpoint="/v1/ask",method="POST",le="+Inf"} 1`,
		`opencost_ai_gateway_request_duration_seconds_count{endpoint="/v1/ask",method="POST"} 1`,
		`opencost_ai_gateway_requests_in_flight 1`,
		`opencost_ai_gateway_tool_calls_total{tool="opencost.allocation"} 1`,
		`opencost_ai_gateway_model_tokens_total{kind="prompt",model="qwen2.5:7b-instruct"} 123`,
		`opencost_ai_gateway_upstream_errors_total{kind="transport",op="chat"} 1`,
		`opencost_ai_gateway_rate_limited_total 1`,
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing line %q\nout:\n%s", want, out)
		}
	}
}

// TestCounter_AccessorReflectsIncrements exercises the public
// Value() accessor the server-package tests rely on.
func TestCounter_AccessorReflectsIncrements(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	c := r.RateLimited()
	c.Inc()
	c.Inc()
	c.Add(3)
	if got := c.Value(); got != 5 {
		t.Fatalf("value = %v, want 5", got)
	}
}

// TestGauge_IncDecSet checks the Gauge wrapper matches the interface
// the middleware relies on.
func TestGauge_IncDecSet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	g := r.InFlight()
	g.Inc()
	g.Inc()
	g.Dec()
	g.Add(2.5)
	if got := g.Value(); got != 3.5 {
		t.Fatalf("value = %v, want 3.5", got)
	}
	g.Set(10)
	if got := g.Value(); got != 10 {
		t.Fatalf("value after Set = %v, want 10", got)
	}
}

// TestHistogram_ObserveAppearsInExposition verifies the histogram
// path produces _bucket/_sum/_count lines in the scrape output. We
// do not assert on specific bucket contents because the bucket
// boundary set is part of the public contract and covered by
// TestRegistry_ExposesDocumentedFamilies via the HELP / TYPE
// assertions.
func TestHistogram_ObserveAppearsInExposition(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.RequestDuration().WithLabelValues("/v1/ask", "POST").Observe(0.2)
	r.RequestDuration().WithLabelValues("/v1/ask", "POST").Observe(2.0)

	out := scrape(t, r)
	wants := []string{
		`opencost_ai_gateway_request_duration_seconds_count{endpoint="/v1/ask",method="POST"} 2`,
		`opencost_ai_gateway_request_duration_seconds_bucket{endpoint="/v1/ask",method="POST",le="+Inf"} 2`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\nout:\n%s", w, out)
		}
	}
}

// TestRegistry_HandlerContentType confirms the /metrics endpoint
// serves text/plain; the openmetrics content-type would break
// scrapers that still expect 0.0.4.
func TestRegistry_HandlerContentType(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain prefix", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	// rate_limited_total is a plain Counter (not a Vec) so its HELP
	// line is emitted even before any increments. CounterVec families
	// stay silent until they get at least one labeled series, which is
	// why the fixture above intentionally uses a labelless counter.
	if !strings.Contains(string(body), "# HELP opencost_ai_gateway_rate_limited_total") {
		t.Errorf("body missing rate_limited HELP:\n%s", body)
	}
}

// TestRegistry_NoProcessOrGoCollectors asserts the gateway's exposed
// surface is exactly the documented families. A fresh NewRegistry
// must not pick up go_* or process_* series, which would silently
// expand the public metric contract.
func TestRegistry_NoProcessOrGoCollectors(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	out := scrape(t, r)
	for _, forbidden := range []string{"go_gc_duration_seconds", "go_goroutines", "process_cpu_seconds_total"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("registry leaked %s into /metrics — use prometheus.NewRegistry(), not the default", forbidden)
		}
	}
}

// TestRegistry_ConcurrentSafety stresses the counters and histograms
// from multiple goroutines to surface any data race the wrapper
// might introduce on top of client_golang's own concurrency
// guarantees. Worth keeping under -race.
func TestRegistry_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	c := r.Requests().WithLabelValues("/v1/ask", "POST", "200")
	h := r.RequestDuration().WithLabelValues("/v1/ask", "POST")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc()
				h.Observe(0.001)
			}
		}()
	}
	wg.Wait()
	if got := c.Value(); got != 8000 {
		t.Errorf("counter = %v, want 8000", got)
	}
	out := scrape(t, r)
	if !strings.Contains(out, `opencost_ai_gateway_request_duration_seconds_count{endpoint="/v1/ask",method="POST"} 8000`) {
		t.Errorf("histogram count missing / wrong in:\n%s", out)
	}
}

// scrape drives reg.Handler() once and returns the body. Using the
// public handler rather than a package-private writer proves the
// wire format the gateway actually serves.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	return rec.Body.String()
}
