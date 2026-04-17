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
)

// writeFlushed writes a JSON value to w followed by a newline and
// flushes if w supports http.Flusher. Mirrors how Ollama actually
// writes a streaming chat response.
func writeFlushed(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := w.Write(append(buf, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestChatStream_HappyPath(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		// Assert stream=true was sent.
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("server decode: %v", err)
		}
		if !req.Stream {
			t.Errorf("server saw stream=false, want true")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)

		writeFlushed(t, w, ChatStreamChunk{Model: "m", Message: Message{Role: "assistant", Content: "hel"}})
		writeFlushed(t, w, ChatStreamChunk{Model: "m", Message: Message{Role: "assistant", Content: "lo"}})
		writeFlushed(t, w, ChatStreamChunk{
			Model:           "m",
			Message:         Message{Role: "assistant", Content: ""},
			Done:            true,
			DoneReason:      "stop",
			PromptEvalCount: 12,
			EvalCount:       34,
		})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := mustClient(t, srv.URL)
	stream, err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var tokens []string
	var final *ChatStreamChunk
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.Done {
			final = chunk
			break
		}
		tokens = append(tokens, chunk.Message.Content)
	}
	if got := strings.Join(tokens, ""); got != "hello" {
		t.Errorf("accumulated tokens = %q, want %q", got, "hello")
	}
	if final == nil {
		t.Fatal("never saw final chunk")
	}
	if final.PromptEvalCount != 12 || final.EvalCount != 34 {
		t.Errorf("final usage = %+v", final)
	}
	if final.DoneReason != "stop" {
		t.Errorf("done_reason = %q", final.DoneReason)
	}
}

func TestChatStream_ToolCallChunk(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeFlushed(t, w, ChatStreamChunk{
			Model: "m",
			Message: Message{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					Function: ToolCallFunction{
						Name:      "opencost.allocation",
						Arguments: map[string]any{"window": "24h"},
					},
				}},
			},
		})
		writeFlushed(t, w, ChatStreamChunk{Model: "m", Message: Message{Role: "tool", Content: "$42"}})
		writeFlushed(t, w, ChatStreamChunk{Done: true, PromptEvalCount: 5, EvalCount: 3})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := mustClient(t, srv.URL)
	stream, err := c.ChatStream(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var sawToolCall, sawToolResult, sawDone bool
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(chunk.Message.ToolCalls) > 0 {
			sawToolCall = true
		}
		if chunk.Message.Role == "tool" {
			sawToolResult = true
		}
		if chunk.Done {
			sawDone = true
		}
	}
	if !sawToolCall {
		t.Error("missed tool_call chunk")
	}
	if !sawToolResult {
		t.Error("missed tool result chunk")
	}
	if !sawDone {
		t.Error("missed done chunk")
	}
}

func TestChatStream_NonJSONStatusReturnsError(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("bridge warming up"))
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := mustClient(t, srv.URL)
	_, err := c.ChatStream(context.Background(), ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	var bErr *Error
	if !errors.As(err, &bErr) {
		t.Fatalf("err not *Error: %v", err)
	}
	if bErr.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", bErr.Status)
	}
	if !strings.Contains(bErr.Body, "bridge warming up") {
		t.Errorf("body = %q", bErr.Body)
	}
}

func TestChatStream_IgnoresBlankLines(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		// Write an initial blank line then a real frame, then another
		// blank line, then the final frame. Blank lines simulate
		// heartbeats some proxies inject between JSON frames.
		_, _ = w.Write([]byte("\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		writeFlushed(t, w, ChatStreamChunk{Message: Message{Role: "assistant", Content: "hi"}})
		_, _ = w.Write([]byte("\n"))
		writeFlushed(t, w, ChatStreamChunk{Done: true})
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := mustClient(t, srv.URL)
	stream, err := c.ChatStream(context.Background(), ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var got []string
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if chunk.Message.Content != "" {
			got = append(got, chunk.Message.Content)
		}
	}
	if len(got) != 1 || got[0] != "hi" {
		t.Errorf("tokens = %v", got)
	}
}
