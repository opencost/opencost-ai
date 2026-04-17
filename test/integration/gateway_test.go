//go:build integration

// Package integration exercises the opencost-ai gateway end-to-end
// against an httptest bridge stub. Build-tagged to keep normal
// `go test ./...` runs fast; run with `go test -tags=integration ./...`.
//
// These tests stand up the full handler chain constructed by
// internal/server, a real *bridge.Client pointed at an in-process
// httptest server that impersonates jonigl/ollama-mcp-bridge, and a
// file-backed *auth.Source. They verify the documented contract:
// SSE streaming, rate limiting, audit log structure, and metric
// increments all behave together.
package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opencost/opencost-ai/internal/audit"
	"github.com/opencost/opencost-ai/internal/auth"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/internal/metrics"
	"github.com/opencost/opencost-ai/internal/ratelimit"
	"github.com/opencost/opencost-ai/internal/server"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// bridgeStub configures the canned responses the bridge httptest
// server will serve. Each field maps to a single endpoint the
// gateway calls during a request.
type bridgeStub struct {
	// chatResp is the non-streaming response served for POST /api/chat
	// when the request body has stream=false.
	chatResp *bridge.ChatResponse
	// streamChunks is served as NDJSON when stream=true.
	streamChunks []bridge.ChatStreamChunk
}

// newBridgeStub stands up an httptest.Server impersonating the bridge.
// The returned cleanup closes the server.
func newBridgeStub(t *testing.T, stub bridgeStub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		// Peek at the body to route stream vs non-stream without
		// duplicating bridge.ChatRequest in this test file.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			enc := json.NewEncoder(w)
			for _, c := range stub.streamChunks {
				if err := enc.Encode(c); err != nil {
					t.Errorf("encode chunk: %v", err)
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(stub.chatResp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tokenFile writes a bearer token to a temp file and returns its path.
// The auth.Source lazily loads the file, so the path must exist when
// the first authenticated request is served.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

// gateway wires the full server.New handler chain the way cmd/gateway
// would in production, but against the stub bridge and a temp-file
// token. Returns handles to the audit buffer and metrics registry so
// tests can assert on them.
type gateway struct {
	url        string
	auditBuf   *bytes.Buffer
	registry   *metrics.Registry
	authToken  string
}

func newGateway(t *testing.T, bridgeURL string, logQuery bool, perMin int) *gateway {
	t.Helper()

	bc, err := bridge.New(bridgeURL)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	const tok = "integration-secret-token"
	validator := auth.NewSource(tokenFile(t, tok))

	auditBuf := &bytes.Buffer{}
	reg := metrics.NewRegistry()
	handler, err := server.New(server.Options{
		Bridge:          bc,
		AuthValidator:   validator,
		DefaultModel:    "qwen2.5:7b-instruct",
		MaxRequestBytes: 8192,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:           audit.NewLogger(auditBuf, logQuery),
		RateLimiter:     ratelimit.New(perMin),
		Metrics:         reg,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &gateway{
		url:       srv.URL,
		auditBuf:  auditBuf,
		registry:  reg,
		authToken: tok,
	}
}

func (g *gateway) post(t *testing.T, body string, stream bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, g.url+"/v1/ask", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+g.authToken)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestIntegration_NonStreaming_FullFlow drives a normal POST /v1/ask
// against the bridge stub and verifies:
//   - the response body carries the model's answer and usage
//   - the audit log emits exactly one line with the correct identity
//   - the requests_total and model_tokens_total counters moved
//   - the /metrics endpoint (served via reg.Handler()) exposes the
//     full schema including the new series
//   - query text is absent from the audit line with logQuery=false
func TestIntegration_NonStreaming_FullFlow(t *testing.T) {
	t.Parallel()
	bridgeSrv := newBridgeStub(t, bridgeStub{
		chatResp: &bridge.ChatResponse{
			Model: "qwen2.5:7b-instruct",
			Message: bridge.Message{
				Role:    "assistant",
				Content: "you spent $42 yesterday",
				ToolCalls: []bridge.ToolCall{{
					Function: bridge.ToolCallFunction{
						Name:      "opencost.allocation",
						Arguments: map[string]any{"window": "24h"},
					},
				}},
			},
			Done:            true,
			PromptEvalCount: 100,
			EvalCount:       20,
		},
	})
	gw := newGateway(t, bridgeSrv.URL, false /* logQuery */, 0 /* no rate limit */)

	resp := gw.post(t, `{"query":"what did i spend yesterday?"}`, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var ar apiv1.AskResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ar.Answer != "you spent $42 yesterday" {
		t.Errorf("answer = %q", ar.Answer)
	}
	if ar.Usage.PromptTokens != 100 || ar.Usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v", ar.Usage)
	}
	if len(ar.ToolCalls) != 1 || ar.ToolCalls[0].Name != "opencost.allocation" {
		t.Errorf("tool_calls = %+v", ar.ToolCalls)
	}

	// Audit: exactly one line, well-formed, no query leak.
	line := strings.TrimSpace(gw.auditBuf.String())
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("expected exactly one audit line, got: %s", line)
	}
	var ev audit.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("decode audit: %v, line=%s", err, line)
	}
	if ev.Status != http.StatusOK || ev.Outcome != "ok" {
		t.Errorf("audit status/outcome = %d/%q", ev.Status, ev.Outcome)
	}
	if ev.Query != "" || ev.Answer != "" {
		t.Errorf("audit leaked query/answer: %+v", ev)
	}
	if ev.CallerIdentity == "" || ev.CallerIdentity == "anonymous" {
		t.Errorf("caller_identity empty/anonymous: %q", ev.CallerIdentity)
	}
	if ev.PromptTokens != 100 || ev.CompletionTokens != 20 {
		t.Errorf("audit tokens = %+v", ev)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].Name != "opencost.allocation" {
		t.Errorf("audit tool_calls = %+v", ev.ToolCalls)
	}

	// Metrics: spot-check the families we just exercised.
	if got := gw.registry.Requests().WithLabelValues("/v1/ask", "POST", "200").Value(); got != 1 {
		t.Errorf("requests_total = %v, want 1", got)
	}
	if got := gw.registry.ModelTokens().WithLabelValues("qwen2.5:7b-instruct", "prompt").Value(); got != 100 {
		t.Errorf("model_tokens prompt = %v, want 100", got)
	}
	if got := gw.registry.ToolCalls().WithLabelValues("opencost.allocation").Value(); got != 1 {
		t.Errorf("tool_calls = %v, want 1", got)
	}

	// The Prometheus text exposition must contain every family, so a
	// fresh scrape reveals the new series to Prometheus.
	metricsSrv := httptest.NewServer(gw.registry.Handler())
	t.Cleanup(metricsSrv.Close)
	m, err := http.Get(metricsSrv.URL)
	if err != nil {
		t.Fatalf("metrics get: %v", err)
	}
	defer m.Body.Close()
	if ct := m.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("metrics content-type = %q", ct)
	}
	mBody, _ := io.ReadAll(m.Body)
	for _, want := range []string{
		"opencost_ai_gateway_requests_total",
		"opencost_ai_gateway_model_tokens_total",
		"opencost_ai_gateway_tool_calls_total",
		"opencost_ai_gateway_rate_limited_total",
	} {
		if !bytes.Contains(mBody, []byte(want)) {
			t.Errorf("metrics exposition missing %s", want)
		}
	}
}

// TestIntegration_Streaming_FullFlow drives a POST /v1/ask with
// stream=true end-to-end: bridge NDJSON is parsed into typed SSE
// events and the final `done` event carries usage + tool_calls.
func TestIntegration_Streaming_FullFlow(t *testing.T) {
	t.Parallel()
	chunks := []bridge.ChatStreamChunk{
		{Model: "qwen2.5:7b-instruct", Thinking: "considering options"},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{
			Role: "assistant",
			ToolCalls: []bridge.ToolCall{{
				Function: bridge.ToolCallFunction{
					Name:      "opencost.allocation",
					Arguments: map[string]any{"window": "24h"},
				},
			}},
		}},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "tool", Content: "$42"}},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "assistant", Content: "you "}},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "assistant", Content: "spent $42"}},
		{
			Model:           "qwen2.5:7b-instruct",
			Done:            true,
			DoneReason:      "stop",
			PromptEvalCount: 50,
			EvalCount:       10,
			TotalDuration:   1_200_000_000,
		},
	}
	bridgeSrv := newBridgeStub(t, bridgeStub{streamChunks: chunks})
	gw := newGateway(t, bridgeSrv.URL, false, 0)

	resp := gw.post(t, `{"query":"what did i spend yesterday?","stream":true}`, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	frames := readSSE(t, resp.Body)
	seen := map[string]int{}
	for _, f := range frames {
		seen[f.event]++
	}
	for _, want := range []string{"thinking", "tool_call", "tool_result", "token", "done"} {
		if seen[want] == 0 {
			t.Errorf("missing event %q; frames=%+v", want, frames)
		}
	}

	var answer strings.Builder
	for _, f := range frames {
		if f.event != "token" {
			continue
		}
		var tok struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(f.data), &tok); err != nil {
			t.Fatalf("decode token: %v", err)
		}
		answer.WriteString(tok.Text)
	}
	if got := answer.String(); got != "you spent $42" {
		t.Errorf("answer = %q", got)
	}

	// Done frame.
	var done struct {
		RequestID string `json:"request_id"`
		Model     string `json:"model"`
		Usage     struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		ToolCalls []struct {
			Name string `json:"name"`
		} `json:"tool_calls"`
	}
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].event == "done" {
			if err := json.Unmarshal([]byte(frames[i].data), &done); err != nil {
				t.Fatalf("decode done: %v", err)
			}
			break
		}
	}
	if done.Usage.PromptTokens != 50 || done.Usage.CompletionTokens != 10 {
		t.Errorf("done usage = %+v", done.Usage)
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].Name != "opencost.allocation" {
		t.Errorf("done tool_calls = %+v", done.ToolCalls)
	}

	// Metrics moved in the streaming path too.
	if got := gw.registry.Requests().WithLabelValues("/v1/ask", "POST", "200").Value(); got != 1 {
		t.Errorf("requests_total = %v, want 1", got)
	}
	if got := gw.registry.ToolCalls().WithLabelValues("opencost.allocation").Value(); got != 1 {
		t.Errorf("tool_calls = %v, want 1", got)
	}

	// Audit: still no query leak with logQuery=false.
	line := strings.TrimSpace(gw.auditBuf.String())
	if strings.Contains(line, "what did i spend yesterday") {
		t.Errorf("audit leaked query: %s", line)
	}
	var ev audit.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if ev.Status != http.StatusOK || ev.Outcome != "ok" {
		t.Errorf("audit status/outcome = %d/%q", ev.Status, ev.Outcome)
	}
}

// TestIntegration_RateLimit_FullFlow verifies that a low per-caller
// limit trips the 429 path end-to-end and leaves a rate_limited
// audit line plus a rate_limited_total metric increment.
func TestIntegration_RateLimit_FullFlow(t *testing.T) {
	t.Parallel()
	bridgeSrv := newBridgeStub(t, bridgeStub{
		chatResp: &bridge.ChatResponse{
			Model:   "qwen2.5:7b-instruct",
			Message: bridge.Message{Role: "assistant", Content: "ok"},
			Done:    true,
		},
	})
	gw := newGateway(t, bridgeSrv.URL, false, 1 /* 1 rpm bucket */)

	first := gw.post(t, `{"query":"a"}`, false)
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d", first.StatusCode)
	}

	second := gw.post(t, `{"query":"b"}`, false)
	defer second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", second.StatusCode)
	}
	body, _ := io.ReadAll(second.Body)
	if !bytes.Contains(body, []byte(`"status":429`)) {
		t.Errorf("429 body not problem+json shape: %s", body)
	}

	if got := gw.registry.RateLimited().Value(); got != 1 {
		t.Errorf("rate_limited_total = %v, want 1", got)
	}

	lines := strings.Split(strings.TrimSpace(gw.auditBuf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want >=2 audit lines, got %d: %s", len(lines), gw.auditBuf.String())
	}
	var last audit.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if last.Outcome != "rate_limited" || last.Status != http.StatusTooManyRequests {
		t.Errorf("last audit = %+v", last)
	}
}

// TestIntegration_OptInQueryCapture flips logQuery=true and verifies
// the gateway records the query on the audit line. This is the one
// code path in the gateway that touches user content by design, so
// it gets a dedicated integration test.
func TestIntegration_OptInQueryCapture(t *testing.T) {
	t.Parallel()
	bridgeSrv := newBridgeStub(t, bridgeStub{
		chatResp: &bridge.ChatResponse{
			Model:   "qwen2.5:7b-instruct",
			Message: bridge.Message{Role: "assistant", Content: "answer"},
			Done:    true,
		},
	})
	gw := newGateway(t, bridgeSrv.URL, true, 0)
	resp := gw.post(t, `{"query":"please capture me"}`, false)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.Contains(gw.auditBuf.String(), "please capture me") {
		t.Errorf("opt-in audit failed to capture query: %s", gw.auditBuf.String())
	}
}

// TestIntegration_ConcurrentStreams stresses the audit + metric wiring
// from several clients at once to surface races that sequential tests
// miss. With -race this is the fastest signal for lock regressions.
func TestIntegration_ConcurrentStreams(t *testing.T) {
	t.Parallel()
	chunks := []bridge.ChatStreamChunk{
		{Model: "m", Message: bridge.Message{Role: "assistant", Content: "hi"}},
		{Model: "m", Done: true, PromptEvalCount: 1, EvalCount: 1},
	}
	bridgeSrv := newBridgeStub(t, bridgeStub{streamChunks: chunks})
	gw := newGateway(t, bridgeSrv.URL, false, 0)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			resp := gw.post(t, `{"query":"hi","stream":true}`, true)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d", resp.StatusCode)
				return
			}
			frames := readSSE(t, resp.Body)
			if len(frames) == 0 || frames[len(frames)-1].event != "done" {
				t.Errorf("missing done frame: %+v", frames)
			}
		}()
	}
	wg.Wait()

	if got := gw.registry.Requests().WithLabelValues("/v1/ask", "POST", "200").Value(); got != n {
		t.Errorf("requests_total = %v, want %d", got, n)
	}

	// There must be exactly one audit line per request — the audit
	// logger serialises writes, so a race would collide the lines.
	lines := strings.Split(strings.TrimSpace(gw.auditBuf.String()), "\n")
	if len(lines) != n {
		t.Fatalf("audit lines = %d, want %d", len(lines), n)
	}
	for i, ln := range lines {
		var ev audit.Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Errorf("decode audit[%d]: %v, line=%s", i, err, ln)
		}
	}
}

// sseFrame is one parsed SSE frame as it arrives over the wire.
type sseFrame struct {
	event string
	data  string
}

// readSSE drains body until EOF, returning each event/data pair. This
// duplicates the parser used in internal/server/stream_test.go on
// purpose: the test suite across packages must not share a helper
// from an internal_test file.
func readSSE(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []sseFrame
	var cur sseFrame
	flush := func() {
		if cur.event != "" || cur.data != "" {
			out = append(out, cur)
		}
		cur = sseFrame{}
	}
	// Set a generous overall deadline so a regression that loses the
	// done frame fails in finite time.
	deadline := time.Now().Add(5 * time.Second)
	for sc.Scan() {
		if time.Now().After(deadline) {
			t.Fatalf("readSSE deadline exceeded")
		}
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
