package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/opencost/opencost-ai/internal/audit"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// sseEvent is one Server-Sent Events frame. The wire format is the
// W3C SSE text protocol: an `event: <type>` line, a single
// `data: <json>` line, and a blank line terminator. The gateway
// always pairs event with a JSON data body — there are no bare
// heartbeat frames, and no multi-line data frames.
type sseEvent struct {
	Type string
	Data any
}

// streamEvent types emitted by POST /v1/ask when stream=true. See
// docs/architecture.md §7.2.
const (
	eventThinking   = "thinking"
	eventToolCall   = "tool_call"
	eventToolResult = "tool_result"
	eventToken      = "token"
	eventDone       = "done"
	eventError      = "error"
)

// thinkingEvent is emitted when the model surfaces intermediate
// reasoning (Ollama's `thinking` field or a role:"thinking" frame).
// The gateway forwards it verbatim; clients that do not want to
// render it can ignore the event type.
type thinkingEvent struct {
	Text string `json:"text"`
}

// toolCallEvent announces an MCP tool invocation. Arguments are
// forwarded so the client can render the intent; no audit log
// consequence.
type toolCallEvent struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// toolResultEvent carries the MCP server's response as relayed back
// through the bridge. The bridge exposes these as role="tool" frames.
type toolResultEvent struct {
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
}

// tokenEvent carries one chunk of the assistant's text.
type tokenEvent struct {
	Text string `json:"text"`
}

// doneEvent is the last frame before the stream closes. Usage +
// final metadata live here so streaming clients can emit a "finished"
// UI without parsing headers or trailing buffers.
type doneEvent struct {
	RequestID     string         `json:"request_id"`
	Model         string         `json:"model"`
	DoneReason    string         `json:"done_reason,omitempty"`
	Usage         apiv1.Usage    `json:"usage"`
	ToolCalls     []apiv1.ToolCall `json:"tool_calls,omitempty"`
	TotalDurationMS int64          `json:"total_duration_ms,omitempty"`
	LatencyMS     int64          `json:"latency_ms"`
}

// errorEvent replaces doneEvent when the stream cannot be completed.
// Status mirrors the HTTP status that would have been used in the
// non-streaming path so clients can reuse their error-mapping code.
type errorEvent struct {
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
}

// askStream is the SSE variant of POST /v1/ask.
//
// The response headers for SSE must be written before the first
// event, so any error discovered after the first Write can only be
// reported inline as an error-typed SSE event — not as an HTTP
// status. The handler tries hard to detect failures before that
// commitment and emit a normal problem+json when it still can.
func (h *handlers) askStream(
	w http.ResponseWriter,
	r *http.Request,
	req apiv1.AskRequest,
	bridgeReq bridge.ChatRequest,
	caller, reqID string,
) {
	// SSE is single-shot — the response body is the stream. A client
	// that cannot accept text/event-stream should not have set
	// stream:true; we don't perform Accept negotiation here beyond
	// a sanity check on Flusher support.
	flusher, ok := w.(http.Flusher)
	if !ok {
		// http.ResponseWriter implementations the gateway is deployed
		// behind all support Flusher; this path is purely a safety
		// net for unexpected middleware that buffers the response.
		writeProblem(w, r, http.StatusInternalServerError,
			problemTitleFor(http.StatusInternalServerError),
			"streaming is not supported by the response writer")
		return
	}

	start := time.Now()
	stream, err := h.bridge.ChatStream(r.Context(), bridgeReq)
	if err != nil {
		h.recordUpstreamError("chat_stream", err)
		// The bridge rejected the request before any bytes left the
		// wire, so we can still return a normal problem+json instead
		// of committing to SSE. bridgeErrorStatus keeps the audited
		// status aligned with whatever mapBridgeError actually writes
		// — without it, a 503 upstream would surface on the wire but
		// get audited as 502.
		status := bridgeErrorStatus(err)
		mapBridgeError(w, r, h.logger, "chat_stream", err)
		h.logAudit(audit.Event{
			RequestID:      reqID,
			CallerIdentity: caller,
			Model:          bridgeReq.Model,
			LatencyMS:      time.Since(start).Milliseconds(),
			Status:         status,
			Outcome:        "error",
			Query:          req.Query,
		})
		return
	}
	defer stream.Close()

	// Commit to SSE. After these headers are flushed, the only way
	// to signal a failure is an inline "event: error" frame.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Defeats buffering proxies (nginx) that would otherwise delay
	// individual frames until they accumulate enough bytes.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)

	// answerBuilder is populated only when the audit logger is
	// configured to capture queries/answers. Buffering every token of
	// a multi-kilobyte streaming reply when the audit log will discard
	// it is pointless memory pressure (and, since those tokens may
	// contain sensitive cost data, also pointless exposure).
	captureAnswer := h.audit.LogQueryEnabled()

	var (
		finalModel       = bridgeReq.Model
		finalReason      string
		promptTokens     int
		completionTokens int
		totalDuration    int64
		tools            []apiv1.ToolCall
		answerBuilder    []byte
	)

	for {
		chunk, nerr := stream.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			h.recordUpstreamError("chat_stream", nerr)
			// The HTTP response is already committed to SSE, so status
			// here is informational — it rides inside the error frame
			// body and the audit event. bridgeErrorStatus keeps both
			// consistent with the non-streaming mapBridgeError path.
			status := bridgeErrorStatus(nerr)
			writeSSE(w, enc, flusher, sseEvent{
				Type: eventError,
				Data: errorEvent{
					RequestID: reqID,
					Status:    status,
					Title:     problemTitleFor(status),
					Detail:    "upstream bridge stream failed",
				},
			})
			h.logAudit(audit.Event{
				RequestID:      reqID,
				CallerIdentity: caller,
				Model:          finalModel,
				LatencyMS:      time.Since(start).Milliseconds(),
				Status:         status,
				Outcome:        "error",
				Query:          req.Query,
			})
			return
		}

		if chunk.Model != "" {
			finalModel = chunk.Model
		}
		if chunk.Thinking != "" {
			writeSSE(w, enc, flusher, sseEvent{
				Type: eventThinking, Data: thinkingEvent{Text: chunk.Thinking},
			})
		}
		for _, tc := range chunk.Message.ToolCalls {
			writeSSE(w, enc, flusher, sseEvent{
				Type: eventToolCall,
				Data: toolCallEvent{Name: tc.Function.Name, Args: tc.Function.Arguments},
			})
			tools = append(tools, apiv1.ToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
		if chunk.Message.Role == "tool" && chunk.Message.Content != "" {
			writeSSE(w, enc, flusher, sseEvent{
				Type: eventToolResult,
				Data: toolResultEvent{Content: chunk.Message.Content},
			})
		}
		if chunk.Message.Role != "tool" && chunk.Message.Content != "" {
			writeSSE(w, enc, flusher, sseEvent{
				Type: eventToken, Data: tokenEvent{Text: chunk.Message.Content},
			})
			if captureAnswer {
				answerBuilder = append(answerBuilder, chunk.Message.Content...)
			}
		}

		if chunk.Done {
			finalReason = chunk.DoneReason
			promptTokens = chunk.PromptEvalCount
			completionTokens = chunk.EvalCount
			totalDuration = chunk.TotalDuration
		}
	}

	latency := time.Since(start)
	done := doneEvent{
		RequestID:       reqID,
		Model:           finalModel,
		DoneReason:      finalReason,
		Usage:           apiv1.Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens},
		ToolCalls:       tools,
		TotalDurationMS: totalDuration / int64(time.Millisecond),
		LatencyMS:       latency.Milliseconds(),
	}
	writeSSE(w, enc, flusher, sseEvent{Type: eventDone, Data: done})

	h.observeTokens(finalModel, promptTokens, completionTokens)
	h.observeToolCalls(tools)
	h.logAudit(audit.Event{
		RequestID:        reqID,
		CallerIdentity:   caller,
		Model:            finalModel,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		ToolCalls:        toAuditToolCalls(tools),
		LatencyMS:        latency.Milliseconds(),
		Status:           http.StatusOK,
		Outcome:          "ok",
		Query:            req.Query,
		Answer:           string(answerBuilder),
	})
}

// writeSSE writes one SSE frame. Any I/O error is silently discarded
// and the frame is abandoned — by the time we are streaming there is
// no meaningful recovery path besides closing the connection, which
// net/http does automatically when the handler returns. A write
// failure here is almost always the client hanging up mid-stream, so
// logging it would add noise without actionable signal.
func writeSSE(w http.ResponseWriter, enc *json.Encoder, flusher http.Flusher, ev sseEvent) {
	// Write the event-type line manually, then delegate the data
	// payload to json.Encoder so we get correct escaping for strings
	// containing newlines or non-ASCII runes.
	if _, err := fmt.Fprintf(w, "event: %s\ndata: ", ev.Type); err != nil {
		return
	}
	if err := enc.Encode(ev.Data); err != nil {
		return
	}
	// json.Encoder already appends a newline; SSE requires a blank
	// line between frames, so one more "\n" closes the frame.
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return
	}
	flusher.Flush()
}
