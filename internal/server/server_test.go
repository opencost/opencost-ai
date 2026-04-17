package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opencost/opencost-ai/internal/auth"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// ---- helpers -------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeValidator is a handwritten auth.Validator for tests.
type fakeValidator struct{ expect string }

func (f fakeValidator) Validate(t string) error {
	if t == f.expect {
		return nil
	}
	return auth.ErrInvalidToken
}

// fakeBridge is a test stub for server.Bridge. Setting chatErr or
// modelsErr makes the corresponding call fail; otherwise the stored
// response is returned verbatim.
type fakeBridge struct {
	chatResp   *bridge.ChatResponse
	chatErr    error
	modelsResp []bridge.TagModel
	modelsErr  error

	lastChatReq bridge.ChatRequest
	chatCalls   int
	modelsCalls int
}

func (f *fakeBridge) Chat(_ context.Context, req bridge.ChatRequest) (*bridge.ChatResponse, error) {
	f.lastChatReq = req
	f.chatCalls++
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	return f.chatResp, nil
}
func (f *fakeBridge) Models(_ context.Context) ([]bridge.TagModel, error) {
	f.modelsCalls++
	if f.modelsErr != nil {
		return nil, f.modelsErr
	}
	return f.modelsResp, nil
}

// newTestServer builds a server.New under test and returns an
// httptest.Server wrapping it. Authorisation header "Bearer secret"
// is accepted by the default stub validator.
func newTestServer(t *testing.T, fb *fakeBridge, opts ...func(*Options)) (*httptest.Server, *fakeBridge) {
	t.Helper()
	if fb == nil {
		fb = &fakeBridge{chatResp: &bridge.ChatResponse{
			Model:   "qwen2.5:7b-instruct",
			Message: bridge.Message{Role: "assistant", Content: "answer"},
			Done:    true,
		}}
	}
	o := Options{
		Bridge:          fb,
		AuthValidator:   fakeValidator{expect: "secret"},
		DefaultModel:    "qwen2.5:7b-instruct",
		MaxRequestBytes: 8192,
		Logger:          discardLogger(),
	}
	for _, f := range opts {
		f(&o)
	}
	h, err := New(o)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, fb
}

func postJSON(t *testing.T, srv *httptest.Server, path, token string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func getJSON(t *testing.T, srv *httptest.Server, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func assertProblemBody(t *testing.T, resp *http.Response, wantStatus int) apiv1.Problem {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, wantStatus, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != apiv1.ProblemContentType {
		t.Errorf("content-type = %q, want %q", ct, apiv1.ProblemContentType)
	}
	var p apiv1.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if p.Status != wantStatus {
		t.Errorf("problem.status = %d, want %d", p.Status, wantStatus)
	}
	return p
}

// ---- wire-up -------------------------------------------------------

func TestNew_RequiresDependencies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		o    Options
		sub  string
	}{
		{"no bridge", Options{AuthValidator: fakeValidator{}, DefaultModel: "m", MaxRequestBytes: 1}, "Bridge"},
		{"no auth", Options{Bridge: &fakeBridge{}, DefaultModel: "m", MaxRequestBytes: 1}, "AuthValidator"},
		{"no default model", Options{Bridge: &fakeBridge{}, AuthValidator: fakeValidator{}, MaxRequestBytes: 1}, "DefaultModel"},
		{"bad max bytes", Options{Bridge: &fakeBridge{}, AuthValidator: fakeValidator{}, DefaultModel: "m"}, "MaxRequestBytes"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.o)
			if err == nil {
				t.Fatalf("want error")
			}
			if !strings.Contains(err.Error(), tc.sub) {
				t.Fatalf("err %q missing %q", err.Error(), tc.sub)
			}
		})
	}
}

// ---- auth middleware applied to every endpoint ---------------------

func TestAllEndpoints_RequireAuth(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)

	cases := []struct {
		name, method, path string
	}{
		{"ask", http.MethodPost, "/v1/ask"},
		{"tools", http.MethodGet, "/v1/tools"},
		{"models", http.MethodGet, "/v1/models"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			assertProblemBody(t, resp, http.StatusUnauthorized)
		})
	}
}

func TestAllEndpoints_RejectBadToken(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	resp := getJSON(t, srv, "/v1/models", "wrong")
	assertProblemBody(t, resp, http.StatusUnauthorized)
}

// ---- POST /v1/ask --------------------------------------------------

func TestAsk_HappyPath(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatResp: &bridge.ChatResponse{
		Model: "qwen2.5:7b-instruct",
		Message: bridge.Message{
			Role: "assistant", Content: "your cluster cost $42",
			ToolCalls: []bridge.ToolCall{{
				Function: bridge.ToolCallFunction{
					Name:      "opencost.allocation",
					Arguments: map[string]any{"window": "24h"},
				},
			}},
		},
		Done:            true,
		PromptEvalCount: 123,
		EvalCount:       45,
	}}
	srv, _ := newTestServer(t, fb)

	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{
		Query: "what did my cluster cost yesterday?",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if rid := resp.Header.Get("X-Request-ID"); rid == "" {
		t.Errorf("X-Request-ID header not set")
	}

	var got apiv1.AskResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Answer != "your cluster cost $42" {
		t.Errorf("answer = %q", got.Answer)
	}
	if got.Model != "qwen2.5:7b-instruct" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Usage.PromptTokens != 123 || got.Usage.CompletionTokens != 45 {
		t.Errorf("usage = %+v", got.Usage)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "opencost.allocation" {
		t.Errorf("tool_calls = %+v", got.ToolCalls)
	}
	if got.RequestID == "" {
		t.Errorf("request_id not populated")
	}
	if fb.lastChatReq.Model != "qwen2.5:7b-instruct" {
		t.Errorf("bridge not given default model: %q", fb.lastChatReq.Model)
	}
	if fb.lastChatReq.Stream {
		t.Errorf("bridge stream forced true")
	}
	if fb.lastChatReq.Messages[0].Content != "what did my cluster cost yesterday?" {
		t.Errorf("bridge messages = %+v", fb.lastChatReq.Messages)
	}
}

func TestAsk_UsesRequestModelOverride(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatResp: &bridge.ChatResponse{Model: "mistral-nemo:12b", Done: true}}
	srv, _ := newTestServer(t, fb)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{
		Query: "hi", Model: "mistral-nemo:12b",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fb.lastChatReq.Model != "mistral-nemo:12b" {
		t.Errorf("model override ignored: %q", fb.lastChatReq.Model)
	}
}

func TestAsk_RejectsWrongContentType(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ask", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertProblemBody(t, resp, http.StatusUnsupportedMediaType)
}

func TestAsk_AcceptsJSONWithCharset(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ask",
		strings.NewReader(`{"query":"hi"}`))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
}

func TestAsk_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ask",
		strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertProblemBody(t, resp, http.StatusBadRequest)
}

// Copilot review on PR #5 flagged that a single dec.Decode call
// accepts "valid JSON ... then garbage" bodies. The ask handler now
// drains a second token and requires io.EOF.
func TestAsk_RejectsTrailingJunk(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	for _, body := range []string{
		`{"query":"hi"} trailing`,          // non-whitespace trailing tokens
		`{"query":"hi"}{"query":"again"}`, // two JSON values
	} {
		body := body
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ask",
				strings.NewReader(body))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			req.Header.Set("Authorization", "Bearer secret")
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			assertProblemBody(t, resp, http.StatusBadRequest)
		})
	}
}

// Whitespace after a valid object is fine (it's not a second JSON
// value). Keep this explicit so a future "strict everything"
// refactor doesn't accidentally reject well-formed trailing
// newlines from curl | json_pp pipelines.
func TestAsk_AcceptsTrailingWhitespace(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ask",
		strings.NewReader(`{"query":"hi"}`+"\n\t "))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, b)
	}
}

func TestAsk_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ask",
		strings.NewReader(`{"query":"hi","unknown":true}`))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertProblemBody(t, resp, http.StatusBadRequest)
}

func TestAsk_EmptyQueryRejected(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "   "})
	p := assertProblemBody(t, resp, http.StatusBadRequest)
	if !strings.Contains(p.Detail, "query is required") {
		t.Errorf("detail = %q", p.Detail)
	}
}

func TestAsk_OverlongQueryRejected(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil, func(o *Options) { o.MaxRequestBytes = 1024 * 1024 })
	big := strings.Repeat("x", maxQueryBytes+1)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: big})
	assertProblemBody(t, resp, http.StatusBadRequest)
}

func TestAsk_EnvelopeTooLarge(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil, func(o *Options) { o.MaxRequestBytes = 64 })
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{
		Query: strings.Repeat("x", 500),
	})
	assertProblemBody(t, resp, http.StatusRequestEntityTooLarge)
}

func TestAsk_BadConversationID(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{
		Query: "hi", ConversationID: "not-a-uuid",
	})
	assertProblemBody(t, resp, http.StatusBadRequest)
}

func TestAsk_StreamRejectedUntilSupported(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{
		Query: "hi", Stream: true,
	})
	p := assertProblemBody(t, resp, http.StatusBadRequest)
	if !strings.Contains(p.Detail, "streaming") {
		t.Errorf("detail = %q", p.Detail)
	}
}

func TestAsk_BridgeTransportFailureMappedTo502(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatErr: &bridge.Error{Op: "chat", Err: errors.New("dial refused")}}
	srv, _ := newTestServer(t, fb)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "hi"})
	p := assertProblemBody(t, resp, http.StatusBadGateway)
	if strings.Contains(p.Detail, "dial refused") {
		t.Errorf("raw upstream error leaked: %q", p.Detail)
	}
}

func TestAsk_BridgeHTTP503MappedTo503(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatErr: &bridge.Error{Op: "chat", Status: http.StatusServiceUnavailable, Body: "loading model"}}
	srv, _ := newTestServer(t, fb)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "hi"})
	p := assertProblemBody(t, resp, http.StatusServiceUnavailable)
	if strings.Contains(p.Detail, "loading model") {
		t.Errorf("upstream body leaked into problem.detail: %q", p.Detail)
	}
}

func TestAsk_NonBridgeErrorStillMaps(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{chatErr: errors.New("mystery")}
	srv, _ := newTestServer(t, fb)
	resp := postJSON(t, srv, "/v1/ask", "secret", apiv1.AskRequest{Query: "hi"})
	p := assertProblemBody(t, resp, http.StatusBadGateway)
	if strings.Contains(p.Detail, "mystery") {
		t.Errorf("raw error leaked: %q", p.Detail)
	}
}

// ---- GET /v1/tools -------------------------------------------------

func TestTools_EmptyListWithDeferralFlag(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	resp := getJSON(t, srv, "/v1/tools", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got apiv1.ToolsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tools) != 0 {
		t.Errorf("tools = %+v, want empty until discovery lands", got.Tools)
	}
	if !got.DiscoveryDeferred {
		t.Errorf("discovery_deferred flag not set")
	}
}

// ---- GET /v1/models ------------------------------------------------

func TestModels_HappyPath(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{modelsResp: []bridge.TagModel{
		{
			Name:   "qwen2.5:7b-instruct",
			Digest: "sha256:abc",
			Size:   4_700_000_000,
			Details: bridge.ModelDetails{
				Family: "qwen2", ParameterSize: "7B", QuantizationLevel: "Q4_K_M",
			},
		},
		{Name: "llama3.1:8b-instruct"},
	}}
	srv, _ := newTestServer(t, fb)

	resp := getJSON(t, srv, "/v1/models", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got apiv1.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("len = %d, want 2", len(got.Models))
	}
	if got.Models[0].Name != "qwen2.5:7b-instruct" {
		t.Errorf("first model name = %q", got.Models[0].Name)
	}
	if got.Models[0].Family != "qwen2" {
		t.Errorf("first model family = %q", got.Models[0].Family)
	}
	if got.Default != "qwen2.5:7b-instruct" {
		t.Errorf("default = %q", got.Default)
	}
}

func TestModels_EmptyListNotAnError(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{modelsResp: nil}
	srv, _ := newTestServer(t, fb)
	resp := getJSON(t, srv, "/v1/models", "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got apiv1.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 0 {
		t.Errorf("len = %d, want 0", len(got.Models))
	}
}

func TestModels_BridgeFailureMapped(t *testing.T) {
	t.Parallel()
	fb := &fakeBridge{modelsErr: &bridge.Error{Op: "models", Err: errors.New("refused")}}
	srv, _ := newTestServer(t, fb)
	resp := getJSON(t, srv, "/v1/models", "secret")
	assertProblemBody(t, resp, http.StatusBadGateway)
}

// ---- request ID propagation ---------------------------------------

func TestRequestID_EchoesCallerSupplied(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/tools", nil)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("X-Request-ID", "trace-1234")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got != "trace-1234" {
		t.Errorf("X-Request-ID = %q, want trace-1234", got)
	}
}

func TestRequestID_GeneratedWhenAbsent(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t, nil)
	resp := getJSON(t, srv, "/v1/tools", "secret")
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got == "" {
		t.Errorf("X-Request-ID not set")
	}
}
