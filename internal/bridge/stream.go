package bridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxStreamLineBytes caps the size of a single NDJSON line the stream
// will accept from upstream. Tool-call argument objects and reasoning
// blocks can be large, so this is generous, but still bounded so a
// rogue upstream cannot exhaust memory with a 1 GiB "line".
const maxStreamLineBytes = 1 << 20

// ChatStream is an iterator over newline-delimited JSON chunks
// produced by POST /api/chat when stream=true. The underlying
// transport is drained on Close.
//
// The iterator is not safe for concurrent use — a single goroutine
// should own the stream from ChatStream construction through Close.
type ChatStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	closed bool
}

func newChatStream(body io.ReadCloser) *ChatStream {
	// Reader buffer is sized so ReadSlice's ErrBufferFull trip point is
	// the documented maxStreamLineBytes ceiling. A 64 KiB default would
	// fire the "stream line exceeded 1 MiB" error at 64 KiB, making the
	// message a lie and the advertised ceiling unenforced. The +1 makes
	// ReadSlice return ErrBufferFull exactly when a line reaches
	// maxStreamLineBytes bytes without a terminating newline.
	r := bufio.NewReaderSize(body, maxStreamLineBytes+1)
	return &ChatStream{body: body, reader: r}
}

// Next returns the next streaming chunk from the bridge. It returns
// io.EOF when the stream is exhausted; any other error indicates a
// transport or framing failure. The returned *ChatStreamChunk must
// not be retained past the subsequent Next call — callers should
// copy the fields they care about into their own struct if they
// need them beyond the next iteration.
func (s *ChatStream) Next() (*ChatStreamChunk, error) {
	if s.closed {
		return nil, io.EOF
	}
	// Loop rather than recurse on blank-line skip: a proxy that emits
	// many consecutive blank-line heartbeats would otherwise grow the
	// call stack one frame per heartbeat and eventually overflow.
	for {
		line, err := s.reader.ReadSlice('\n')
		// ReadSlice returns ErrBufferFull when a single line exceeds the
		// buffer size. Convert that into a well-typed error so callers
		// can tell "malformed stream" from "remote hung up".
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, &Error{Op: "chat_stream", Err: fmt.Errorf("stream line exceeded %d bytes", maxStreamLineBytes)}
		}
		if err != nil && err != io.EOF {
			return nil, &Error{Op: "chat_stream", Err: err}
		}
		// Strip the trailing newline. ReadSlice may also return a final
		// fragment with io.EOF and no newline; handle both.
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			if err == io.EOF {
				return nil, io.EOF
			}
			// Some proxies inject heartbeat blank lines between JSON
			// frames; skip them rather than bubbling up as a decode
			// error.
			continue
		}

		var chunk ChatStreamChunk
		if jerr := json.Unmarshal(line, &chunk); jerr != nil {
			return nil, &Error{Op: "chat_stream", Err: fmt.Errorf("decode chunk: %w", jerr)}
		}
		return &chunk, nil
	}
}

// Close drains and closes the underlying response body. It is safe to
// call multiple times.
func (s *ChatStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	// Drain any unread bytes so the underlying connection can be
	// returned to the pool; ignore errors on drain because we are
	// shutting down the stream anyway.
	_, _ = io.Copy(io.Discard, io.LimitReader(s.body, 1<<16))
	return s.body.Close()
}

// ChatStreamChunk is one NDJSON frame emitted by the bridge during a
// streaming /api/chat. The Ollama shape allows each frame to carry
// zero or more of: message content (token fragment), tool calls (on
// an intermediate assistant turn), a tool-role message (MCP result
// echoed back), or an end-of-stream marker with usage stats.
type ChatStreamChunk struct {
	// Model echoes the model name the bridge routed to. Present on
	// every frame.
	Model string `json:"model,omitempty"`

	// Message carries the incremental assistant output plus any
	// tool-call metadata emitted during this frame.
	Message Message `json:"message"`

	// Thinking is Ollama's reasoning-model extension (qwen3, deepseek-r1,
	// etc.) that emits private chain-of-thought between rounds. When
	// present it is semantically distinct from Message.Content and
	// the gateway surfaces it as a `thinking` SSE event.
	Thinking string `json:"thinking,omitempty"`

	// Done is true on the final frame of a stream. The final frame
	// also carries total token counts.
	Done bool `json:"done"`

	// DoneReason, when present on the final frame, explains why the
	// stream ended ("stop", "length", "tool_calls", ...).
	DoneReason string `json:"done_reason,omitempty"`

	// PromptEvalCount is reported only on the final frame.
	PromptEvalCount int `json:"prompt_eval_count,omitempty"`

	// EvalCount is reported only on the final frame.
	EvalCount int `json:"eval_count,omitempty"`

	// TotalDuration, LoadDuration, PromptEvalDuration, EvalDuration are
	// the Ollama timing fields, all in nanoseconds. The gateway does
	// not surface them individually but forwards total_duration via
	// the done SSE event for client-side progress UIs.
	TotalDuration      int64 `json:"total_duration,omitempty"`
	LoadDuration       int64 `json:"load_duration,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalDuration       int64 `json:"eval_duration,omitempty"`
}
