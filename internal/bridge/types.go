package bridge

import (
	"time"
)

// Message is one entry in a chat conversation. Mirrors the Ollama
// /api/chat message shape — role in {"system","user","assistant",
// "tool"}, a content string, and optional tool-call metadata on
// assistant messages.
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall is a single tool invocation emitted by the model, as
// reported back in the chat response. The bridge has already
// executed the tool by the time we see it; Function.Arguments is
// the argument object the model passed in.
type ToolCall struct {
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the name + arguments of a tool call. Kept
// separate from ToolCall because Ollama wraps function calls in a
// named object, leaving room for future non-function call types.
type ToolCallFunction struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ChatRequest is the body we send to POST /api/chat. Stream is
// hard-coded false in v0.1 (see Client.Chat); the field is present
// on the wire so callers reading this type understand the full
// contract.
type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}

// ChatResponse is the non-streaming response from POST /api/chat.
// Field names mirror Ollama's contract verbatim so future callers
// grepping the Ollama docs find the same identifiers here.
type ChatResponse struct {
	Model              string    `json:"model"`
	CreatedAt          time.Time `json:"created_at"`
	Message            Message   `json:"message"`
	Done               bool      `json:"done"`
	DoneReason         string    `json:"done_reason,omitempty"`
	TotalDuration      int64     `json:"total_duration,omitempty"`
	LoadDuration       int64     `json:"load_duration,omitempty"`
	PromptEvalCount    int       `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64     `json:"prompt_eval_duration,omitempty"`
	EvalCount          int       `json:"eval_count,omitempty"`
	EvalDuration       int64     `json:"eval_duration,omitempty"`
}

// TagModel is one entry in GET /api/tags. Exposed so callers can
// render the list without re-parsing JSON.
type TagModel struct {
	Name       string       `json:"name"`
	Model      string       `json:"model"`
	ModifiedAt time.Time    `json:"modified_at"`
	Size       int64        `json:"size"`
	Digest     string       `json:"digest"`
	Details    ModelDetails `json:"details"`
}

// ModelDetails is Ollama's nested per-model descriptor. Fields
// tracked here are the ones operators actually render; the bridge
// may add more and we let encoding/json drop them on decode.
type ModelDetails struct {
	Format            string   `json:"format,omitempty"`
	Family            string   `json:"family,omitempty"`
	Families          []string `json:"families,omitempty"`
	ParameterSize     string   `json:"parameter_size,omitempty"`
	QuantizationLevel string   `json:"quantization_level,omitempty"`
}

// tagsResponse is the on-the-wire envelope for /api/tags. Not
// exported because callers want the slice, not the envelope.
type tagsResponse struct {
	Models []TagModel `json:"models"`
}

// Error is the problem-style envelope used for upstream failures.
// The gateway maps this into an RFC 7807 Problem before returning
// it to the client, so the string form is intentionally terse.
type Error struct {
	// Status is the HTTP status returned by the bridge, or 0 for
	// transport-level errors (connection refused, DNS failure).
	Status int
	// Op identifies the operation that failed ("chat", "models").
	Op string
	// Body is a truncated copy of the upstream response body, for
	// operator debugging. Never surfaced verbatim to API clients.
	Body string
	// Err is the wrapped underlying error when the failure was not
	// a well-formed HTTP response (e.g. context cancelled, I/O).
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return "bridge " + e.Op + ": " + e.Err.Error()
	}
	return "bridge " + e.Op + ": upstream status " + itoa(e.Status)
}

func (e *Error) Unwrap() error { return e.Err }

// itoa is a tiny local helper so Error() does not import strconv
// just to stringify a status code; keeps the package's stdlib
// surface minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

