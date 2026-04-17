package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeBridge is a test-only upstream that asserts on the incoming
// request and returns a canned response. Each handler registers
// itself for a specific path so a single httptest.Server can exercise
// both /api/chat and /api/tags without interfering.
type fakeBridge struct {
	chat http.HandlerFunc
	tags http.HandlerFunc
}

func (f *fakeBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/chat":
		f.chat(w, r)
	case "/api/tags":
		f.tags(w, r)
	default:
		http.NotFound(w, r)
	}
}

func newBridgeServer(t *testing.T, f *fakeBridge) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func mustClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(baseURL)
	if err != nil {
		t.Fatalf("New(%q): %v", baseURL, err)
	}
	return c
}

func TestNew_RejectsInvalidURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		url     string
		wantSub string
	}{
		{"empty", "", "scheme must be http or https"},
		{"no scheme", "//host", "scheme must be http or https"},
		{"ftp scheme", "ftp://host", "scheme must be http or https"},
		{"no host", "http://", "missing host"},
		{"bad parse", "http://[::1", "parse bridge URL"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.url)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q missing %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	c, err := New("http://example.com/bridge/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.BaseURL(); got != "http://example.com/bridge" {
		t.Fatalf("BaseURL = %q, want trimmed", got)
	}
}

// Exercises the path-prefix fix from Copilot review on PR #5: when
// the operator points the gateway at http://host/bridge, the client
// must call /bridge/api/chat and /bridge/api/tags, not /api/chat and
// /api/tags. Previously (*url.URL).Parse stripped the prefix.
func TestClient_PreservesBasePathPrefix(t *testing.T) {
	t.Parallel()
	var chatPath, tagsPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/bridge/api/chat", func(w http.ResponseWriter, r *http.Request) {
		chatPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(ChatResponse{Model: "m", Done: true})
	})
	mux.HandleFunc("/bridge/api/tags", func(w http.ResponseWriter, r *http.Request) {
		tagsPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(tagsResponse{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Include a trailing slash to confirm normalisation does not
	// re-introduce the bug.
	c := mustClient(t, srv.URL+"/bridge/")

	if _, err := c.Chat(context.Background(), ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("Chat with prefix: %v", err)
	}
	if chatPath != "/bridge/api/chat" {
		t.Errorf("chat reached %q, want /bridge/api/chat", chatPath)
	}
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models with prefix: %v", err)
	}
	if tagsPath != "/bridge/api/tags" {
		t.Errorf("models reached %q, want /bridge/api/tags", tagsPath)
	}
}

func TestChat_Success(t *testing.T) {
	t.Parallel()
	f := &fakeBridge{
		chat: func(w http.ResponseWriter, r *http.Request) {
			// Validate what the gateway actually sends.
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q", ct)
			}
			if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "opencost-ai-gateway/") {
				t.Errorf("user-agent = %q", ua)
			}
			var req ChatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			if req.Stream {
				t.Errorf("stream must be forced to false, got true")
			}
			if req.Model != "qwen2.5:7b-instruct" {
				t.Errorf("model = %q", req.Model)
			}
			if len(req.Messages) != 1 || req.Messages[0].Content != "hello" {
				t.Errorf("messages = %+v", req.Messages)
			}
			_ = json.NewEncoder(w).Encode(ChatResponse{
				Model:           "qwen2.5:7b-instruct",
				CreatedAt:       time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC),
				Message:         Message{Role: "assistant", Content: "hi back"},
				Done:            true,
				PromptEvalCount: 10,
				EvalCount:       3,
			})
		},
	}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "qwen2.5:7b-instruct",
		Messages: []Message{{Role: "user", Content: "hello"}},
		Stream:   true, // client must override this
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "hi back" {
		t.Errorf("content = %q", resp.Message.Content)
	}
	if resp.EvalCount != 3 {
		t.Errorf("eval_count = %d", resp.EvalCount)
	}
}

func TestChat_DecodesToolCalls(t *testing.T) {
	t.Parallel()
	// Round-trip an assistant message that invoked two tools. This
	// pins the JSON shape we expect from the bridge so a change in
	// Ollama's tool_call envelope surfaces here rather than at the
	// /v1/ask handler boundary.
	raw := `{
	  "model":"m",
	  "created_at":"2026-04-17T00:00:00Z",
	  "message":{
	    "role":"assistant",
	    "content":"done",
	    "tool_calls":[
	      {"function":{"name":"opencost.allocation","arguments":{"window":"7d"}}},
	      {"function":{"name":"opencost.asset","arguments":{}}}
	    ]
	  },
	  "done":true
	}`
	f := &fakeBridge{chat: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw)
	}}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	resp, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := len(resp.Message.ToolCalls); got != 2 {
		t.Fatalf("tool_calls = %d, want 2", got)
	}
	if got := resp.Message.ToolCalls[0].Function.Name; got != "opencost.allocation" {
		t.Errorf("first tool call name = %q", got)
	}
	if got := resp.Message.ToolCalls[0].Function.Arguments["window"]; got != "7d" {
		t.Errorf("first tool call arg window = %v", got)
	}
}

func TestChat_UpstreamError(t *testing.T) {
	t.Parallel()
	f := &fakeBridge{chat: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"detail":"ollama unreachable"}`)
	}}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	_, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	var bridgeErr *Error
	if !errors.As(err, &bridgeErr) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if bridgeErr.Status != http.StatusBadGateway {
		t.Errorf("status = %d", bridgeErr.Status)
	}
	if bridgeErr.Op != "chat" {
		t.Errorf("op = %q", bridgeErr.Op)
	}
	if !strings.Contains(bridgeErr.Body, "ollama unreachable") {
		t.Errorf("body = %q", bridgeErr.Body)
	}
	if !strings.Contains(err.Error(), "upstream status 502") {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestChat_TransportFailure(t *testing.T) {
	t.Parallel()
	// Close the server before calling — any subsequent Dial fails.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	c := mustClient(t, srv.URL)
	_, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	var bridgeErr *Error
	if !errors.As(err, &bridgeErr) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if bridgeErr.Err == nil {
		t.Errorf("Err should be set on transport failure")
	}
	if bridgeErr.Status != 0 {
		t.Errorf("status = %d, want 0 for transport error", bridgeErr.Status)
	}
}

func TestChat_ContextCancel(t *testing.T) {
	t.Parallel()
	// Upstream never responds; client ctx cancel must propagate as a
	// transport error without hanging.
	done := make(chan struct{})
	f := &fakeBridge{chat: func(w http.ResponseWriter, r *http.Request) {
		<-done
	}}
	srv := newBridgeServer(t, f)
	t.Cleanup(func() { close(done) })

	c := mustClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.Chat(ctx, ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("want error on ctx cancel, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded in chain, got %v", err)
	}
}

func TestChat_GarbageResponseBody(t *testing.T) {
	t.Parallel()
	f := &fakeBridge{chat: func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	_, err := c.Chat(context.Background(), ChatRequest{Model: "m"})
	var bridgeErr *Error
	if !errors.As(err, &bridgeErr) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected decode failure, got %v", err)
	}
}

func TestModels_Success(t *testing.T) {
	t.Parallel()
	f := &fakeBridge{tags: func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(tagsResponse{
			Models: []TagModel{
				{
					Name:       "qwen2.5:7b-instruct",
					Model:      "qwen2.5:7b-instruct",
					ModifiedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
					Size:       4_700_000_000,
					Digest:     "sha256:abc",
					Details: ModelDetails{
						Format:            "gguf",
						Family:            "qwen2",
						ParameterSize:     "7B",
						QuantizationLevel: "Q4_K_M",
					},
				},
			},
		})
	}}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Name != "qwen2.5:7b-instruct" {
		t.Errorf("name = %q", got[0].Name)
	}
	if got[0].Details.Family != "qwen2" {
		t.Errorf("family = %q", got[0].Details.Family)
	}
}

func TestModels_EmptyList(t *testing.T) {
	t.Parallel()
	// Fresh install: Ollama returns {"models":[]}. Client must
	// return ([], nil), not an error — the UI decides whether
	// "no models" is a problem.
	f := &fakeBridge{tags: func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[]}`)
	}}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestModels_UpstreamError(t *testing.T) {
	t.Parallel()
	f := &fakeBridge{tags: func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ollama down", http.StatusServiceUnavailable)
	}}
	srv := newBridgeServer(t, f)
	c := mustClient(t, srv.URL)

	_, err := c.Models(context.Background())
	var bridgeErr *Error
	if !errors.As(err, &bridgeErr) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if bridgeErr.Op != "models" {
		t.Errorf("op = %q", bridgeErr.Op)
	}
	if bridgeErr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d", bridgeErr.Status)
	}
}

