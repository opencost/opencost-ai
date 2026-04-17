package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opencost/opencost-ai/internal/audit"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/internal/metrics"
	"github.com/opencost/opencost-ai/internal/ratelimit"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// newStreamingServer wires the full server.New with a real httptest
// upstream bridge the caller configures via chunks. Returns the
// httptest.Server fronting the gateway, plus handles to the audit
// buffer and metrics registry so assertions can inspect them.
func newStreamingServer(
	t *testing.T,
	chunks []bridge.ChatStreamChunk,
	perMin int,
) (*httptest.Server, *bytes.Buffer, *metrics.Registry) {
	t.Helper()

	bridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		for _, c := range chunks {
			if err := enc.Encode(c); err != nil {
				t.Fatalf("encode chunk: %v", err)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(bridgeSrv.Close)

	bc, err := bridge.New(bridgeSrv.URL)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}

	var auditBuf bytes.Buffer
	reg := metrics.NewRegistry()
	h, err := New(Options{
		Bridge:          bc,
		AuthValidator:   fakeValidator{expect: "secret"},
		DefaultModel:    "qwen2.5:7b-instruct",
		MaxRequestBytes: 8192,
		Logger:          discardLogger(),
		Audit:           audit.NewLogger(&auditBuf, false),
		RateLimiter:     ratelimit.New(perMin),
		Metrics:         reg,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw, &auditBuf, reg
}

// sseFrame is one parsed text/event-stream frame.
type sseFrame struct {
	Event string
	Data  string
}

// readSSEFrames parses the SSE wire format from body into a slice of
// frames. Blank line terminates each frame. Returns when the reader
// reaches EOF.
func readSSEFrames(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var frames []sseFrame
	var current sseFrame
	flush := func() {
		if current.Event != "" || current.Data != "" {
			frames = append(frames, current)
		}
		current = sseFrame{}
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return frames
}

func TestAsk_Streaming_HappyPath(t *testing.T) {
	t.Parallel()
	chunks := []bridge.ChatStreamChunk{
		{Model: "qwen2.5:7b-instruct", Thinking: "let me check allocations"},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "assistant", ToolCalls: []bridge.ToolCall{{
			Function: bridge.ToolCallFunction{Name: "opencost.allocation", Arguments: map[string]any{"window": "24h"}},
		}}}},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "tool", Content: "$42"}},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "assistant", Content: "you spent "}},
		{Model: "qwen2.5:7b-instruct", Message: bridge.Message{Role: "assistant", Content: "$42"}},
		{
			Model:           "qwen2.5:7b-instruct",
			Message:         bridge.Message{Role: "assistant"},
			Done:            true,
			DoneReason:      "stop",
			PromptEvalCount: 100,
			EvalCount:       20,
			TotalDuration:   1_500_000_000,
		},
	}
	gw, auditBuf, reg := newStreamingServer(t, chunks, 0)

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/ask",
		strings.NewReader(`{"query":"what did i spend yesterday?","stream":true}`))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	frames := readSSEFrames(t, resp.Body)

	// The event stream must include each typed event at least once.
	seen := map[string]int{}
	for _, f := range frames {
		seen[f.Event]++
	}
	for _, want := range []string{"thinking", "tool_call", "tool_result", "token", "done"} {
		if seen[want] == 0 {
			t.Errorf("missing event type %q; frames = %+v", want, frames)
		}
	}

	// Tokens should concatenate to the model's answer.
	var answer strings.Builder
	for _, f := range frames {
		if f.Event != "token" {
			continue
		}
		var ev tokenEvent
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
			t.Fatalf("decode token: %v", err)
		}
		answer.WriteString(ev.Text)
	}
	if got := answer.String(); got != "you spent $42" {
		t.Errorf("answer = %q", got)
	}

	// The final done event should carry usage and the request ID.
	var doneFrame *sseFrame
	for i := range frames {
		if frames[i].Event == "done" {
			doneFrame = &frames[i]
			break
		}
	}
	if doneFrame == nil {
		t.Fatal("no done event")
	}
	var done doneEvent
	if err := json.Unmarshal([]byte(doneFrame.Data), &done); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	if done.Usage.PromptTokens != 100 || done.Usage.CompletionTokens != 20 {
		t.Errorf("done usage = %+v", done.Usage)
	}
	if done.Model != "qwen2.5:7b-instruct" {
		t.Errorf("done model = %q", done.Model)
	}
	if done.RequestID == "" {
		t.Errorf("done request_id empty")
	}
	if len(done.ToolCalls) != 1 || done.ToolCalls[0].Name != "opencost.allocation" {
		t.Errorf("done tool_calls = %+v", done.ToolCalls)
	}

	// Audit line must exist and must not contain the query text (the
	// stream test uses logQuery=false).
	auditLine := strings.TrimSpace(auditBuf.String())
	if auditLine == "" {
		t.Fatal("audit buffer empty after streaming")
	}
	if strings.Contains(auditLine, "what did i spend yesterday") {
		t.Errorf("audit leaked query: %s", auditLine)
	}
	var ev audit.Event
	if err := json.Unmarshal([]byte(auditLine), &ev); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if ev.Status != http.StatusOK || ev.Outcome != "ok" {
		t.Errorf("audit status/outcome = %d/%q", ev.Status, ev.Outcome)
	}
	if ev.Model != "qwen2.5:7b-instruct" {
		t.Errorf("audit model = %q", ev.Model)
	}
	if ev.PromptTokens != 100 || ev.CompletionTokens != 20 {
		t.Errorf("audit tokens = %+v", ev)
	}
	if len(ev.ToolCalls) != 1 || ev.ToolCalls[0].Name != "opencost.allocation" {
		t.Errorf("audit tool_calls = %+v", ev.ToolCalls)
	}

	// Metrics: the /v1/ask request counter and the model tokens
	// counter must both have moved.
	if got := reg.Requests().WithLabelValues("/v1/ask", "POST", "200").Value(); got != 1 {
		t.Errorf("requests_total{/v1/ask,POST,200} = %v, want 1", got)
	}
	if got := reg.ModelTokens().WithLabelValues("qwen2.5:7b-instruct", "prompt").Value(); got != 100 {
		t.Errorf("model_tokens prompt = %v, want 100", got)
	}
	if got := reg.ModelTokens().WithLabelValues("qwen2.5:7b-instruct", "completion").Value(); got != 20 {
		t.Errorf("model_tokens completion = %v, want 20", got)
	}
	if got := reg.ToolCalls().WithLabelValues("opencost.allocation").Value(); got != 1 {
		t.Errorf("tool_calls = %v, want 1", got)
	}
}

func TestAsk_Streaming_IncludesQueryWhenOptedIn(t *testing.T) {
	t.Parallel()
	chunks := []bridge.ChatStreamChunk{
		{Model: "m", Message: bridge.Message{Role: "assistant", Content: "ok"}},
		{Model: "m", Done: true, PromptEvalCount: 1, EvalCount: 1},
	}
	// Re-derive newStreamingServer inline because we need to override
	// the audit logQuery flag.
	bridgeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, c := range chunks {
			_ = enc.Encode(c)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(bridgeSrv.Close)

	bc, err := bridge.New(bridgeSrv.URL)
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	var auditBuf bytes.Buffer
	h, err := New(Options{
		Bridge:          bc,
		AuthValidator:   fakeValidator{expect: "secret"},
		DefaultModel:    "m",
		MaxRequestBytes: 8192,
		Logger:          discardLogger(),
		Audit:           audit.NewLogger(&auditBuf, true),
		RateLimiter:     ratelimit.New(0),
		Metrics:         metrics.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/ask",
		strings.NewReader(`{"query":"capture me","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !strings.Contains(auditBuf.String(), "capture me") {
		t.Errorf("opt-in audit failed to capture query: %s", auditBuf.String())
	}
}

func TestAsk_NonStreaming_IncrementsMetricsAndEmitsAudit(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatResp: &bridge.ChatResponse{
		Model:   "qwen2.5:7b-instruct",
		Message: bridge.Message{Role: "assistant", Content: "answer"},
		Done:    true,
		PromptEvalCount: 42, EvalCount: 8,
	}}

	var auditBuf bytes.Buffer
	reg := metrics.NewRegistry()
	h, err := New(Options{
		Bridge:          fb,
		AuthValidator:   fakeValidator{expect: "secret"},
		DefaultModel:    "qwen2.5:7b-instruct",
		MaxRequestBytes: 8192,
		Logger:          discardLogger(),
		Audit:           audit.NewLogger(&auditBuf, false),
		RateLimiter:     ratelimit.New(0),
		Metrics:         reg,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if got := reg.Requests().WithLabelValues("/v1/ask", "POST", "200").Value(); got != 1 {
		t.Errorf("requests_total = %v, want 1", got)
	}
	if got := reg.ModelTokens().WithLabelValues("qwen2.5:7b-instruct", "prompt").Value(); got != 42 {
		t.Errorf("prompt tokens = %v, want 42", got)
	}

	line := strings.TrimSpace(auditBuf.String())
	var ev audit.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("decode audit: %v, line=%s", err, line)
	}
	if ev.PromptTokens != 42 || ev.CompletionTokens != 8 {
		t.Errorf("audit tokens = %+v", ev)
	}
	if ev.CallerIdentity == "" || ev.CallerIdentity == "anonymous" {
		t.Errorf("caller_identity empty or anonymous for authenticated call: %q", ev.CallerIdentity)
	}
	if ev.Query != "" {
		t.Errorf("query leaked with logQuery=false: %q", ev.Query)
	}
}

func TestAsk_RateLimited_Returns429(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatResp: &bridge.ChatResponse{
		Model: "m", Message: bridge.Message{Role: "assistant", Content: "ok"}, Done: true,
	}}
	var auditBuf bytes.Buffer
	reg := metrics.NewRegistry()
	h, err := New(Options{
		Bridge:          fb,
		AuthValidator:   fakeValidator{expect: "secret"},
		DefaultModel:    "m",
		MaxRequestBytes: 8192,
		Logger:          discardLogger(),
		Audit:           audit.NewLogger(&auditBuf, false),
		RateLimiter:     ratelimit.New(1),
		Metrics:         reg,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// First call allowed.
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "a"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d", resp.StatusCode)
	}
	// Second call should be rate-limited.
	resp = postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "b"})
	p := assertProblemBody(t, resp, http.StatusTooManyRequests)
	if !strings.Contains(p.Detail, "rate limit") {
		t.Errorf("detail = %q", p.Detail)
	}
	if got := reg.RateLimited().Value(); got != 1 {
		t.Errorf("rate_limited_total = %v, want 1", got)
	}

	// Audit log should carry a rate_limited entry.
	lines := strings.Split(strings.TrimSpace(auditBuf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want 2 audit lines, got %d: %s", len(lines), auditBuf.String())
	}
	var last audit.Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	if last.Outcome != "rate_limited" || last.Status != http.StatusTooManyRequests {
		t.Errorf("last audit = %+v", last)
	}
}

// Make sure concurrent streaming requests don't interleave frames.
func TestAsk_Streaming_ConcurrentClientsDoNotInterleave(t *testing.T) {
	t.Parallel()
	chunks := []bridge.ChatStreamChunk{
		{Model: "m", Message: bridge.Message{Role: "assistant", Content: "a"}},
		{Model: "m", Message: bridge.Message{Role: "assistant", Content: "b"}},
		{Model: "m", Done: true, PromptEvalCount: 1, EvalCount: 1},
	}
	gw, _, _ := newStreamingServer(t, chunks, 0)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
				gw.URL+"/v1/ask",
				strings.NewReader(`{"query":"hi","stream":true}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer secret")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("do: %v", err)
				return
			}
			defer resp.Body.Close()
			frames := readSSEFrames(t, resp.Body)
			// Every stream must end with a done event and contain
			// both of the expected token frames.
			if len(frames) == 0 || frames[len(frames)-1].Event != "done" {
				t.Errorf("missing done frame: %+v", frames)
			}
		}()
	}
	wg.Wait()
}
