package apiv1

// Format names the shape of the answer field in AskResponse.
//
// "text" returns a markdown string; "json" asks the gateway to coerce the
// model output into structured JSON suitable for UI consumption. The set
// is intentionally closed: unknown values are rejected at the boundary
// rather than silently downgraded.
type Format string

const (
	// FormatText returns the answer as a markdown-formatted string.
	FormatText Format = "text"

	// FormatJSON returns the answer as a JSON object; the gateway is
	// responsible for schema enforcement.
	FormatJSON Format = "json"
)

// AskRequest is the body of POST /v1/ask.
//
// Field semantics mirror docs/architecture.md §7.2. All validation
// (length, enum membership, UUID well-formedness) happens in the
// gateway — the types here describe the wire format only.
type AskRequest struct {
	// Query is the user-supplied natural-language question. Required.
	// Maximum length is enforced by the gateway; see
	// OPENCOST_AI_MAX_REQUEST_BYTES for the envelope limit and the
	// documented 4 KiB ceiling for Query itself.
	Query string `json:"query"`

	// Model optionally overrides the server-configured default model.
	// When empty the gateway substitutes OPENCOST_AI_DEFAULT_MODEL.
	Model string `json:"model,omitempty"`

	// Stream, when true, switches the response to Server-Sent Events.
	// Defaults to false.
	Stream bool `json:"stream,omitempty"`

	// Format selects the answer encoding. Empty is treated as
	// FormatText.
	Format Format `json:"format,omitempty"`

	// ConversationID threads a request onto an existing multi-turn
	// conversation. Must be a UUID when non-empty.
	ConversationID string `json:"conversation_id,omitempty"`
}

// ToolCall describes a single MCP tool invocation performed by the model
// while answering a request. It is reported back to the caller so they
// can audit which cost-data endpoints were touched.
type ToolCall struct {
	// Name is the fully-qualified MCP tool name, e.g. "opencost.allocation".
	Name string `json:"name"`

	// Args is the argument object passed to the tool. The gateway does
	// not attempt to schema-enforce this; it is echoed verbatim.
	Args map[string]any `json:"args,omitempty"`

	// DurationMS is the wall-clock duration of the tool call in
	// milliseconds, as measured by the gateway.
	DurationMS int64 `json:"duration_ms"`
}

// Usage reports token accounting for a single AskResponse.
type Usage struct {
	// PromptTokens counts tokens in the composed prompt (system prompt
	// + conversation + user query + tool results).
	PromptTokens int `json:"prompt_tokens"`

	// CompletionTokens counts tokens emitted by the model.
	CompletionTokens int `json:"completion_tokens"`
}

// AskResponse is the non-streaming response body for POST /v1/ask.
//
// The streaming counterpart is SSE-framed; see docs/architecture.md §7.2
// for the event shapes (thinking, tool_call, tool_result, token, done).
type AskResponse struct {
	// RequestID is the server-assigned UUID for this request. It is
	// also emitted in the audit log and in response headers.
	RequestID string `json:"request_id"`

	// Model names the model that actually served the request (may
	// differ from the requested model if the default was substituted).
	Model string `json:"model"`

	// Query echoes the caller's query so clients rendering a log do
	// not need to correlate by RequestID alone.
	Query string `json:"query"`

	// Answer is the model-produced response, encoded per the request's
	// Format field.
	Answer string `json:"answer"`

	// ToolCalls lists the MCP tools invoked while producing Answer, in
	// call order. May be empty when the model answered from context
	// alone.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Usage reports prompt and completion token totals.
	Usage Usage `json:"usage"`

	// LatencyMS is the end-to-end wall-clock time spent inside the
	// gateway, in milliseconds.
	LatencyMS int64 `json:"latency_ms"`
}

// HealthResponse is the body of GET /v1/health.
//
// In the v0.1 scaffold this endpoint is liveness-only: Status is "ok"
// while the process is up and the HTTP listener is accepting
// connections. Readiness — whether the configured bridge, Ollama,
// and OpenCost MCP are reachable — arrives with the internal/bridge
// client and will populate Status "degraded" when any upstream is
// unreachable. Callers must treat unknown Status values as degraded.
//
// Readiness probes in Kubernetes should therefore point at a future
// /v1/ready endpoint, not /v1/health, until that work lands. See
// docs/architecture.md §7.1.
type HealthResponse struct {
	// Status is "ok" (liveness OK) or "degraded" (reserved for the
	// readiness-aware implementation). Callers must treat unknown
	// values as degraded.
	Status string `json:"status"`

	// Version is the gateway build version (semver or git describe).
	Version string `json:"version,omitempty"`
}

// Problem is an RFC 7807 problem+json error document.
//
// The gateway returns Problem for every non-2xx response. Fields map
// directly to RFC 7807 §3.1. Implementations must set the response
// Content-Type to "application/problem+json".
//
// See docs/architecture.md §7.5. The gateway never returns raw
// exception strings; Detail is a human-readable, caller-safe summary.
type Problem struct {
	// Type is a URI reference that identifies the problem type. When
	// omitted it defaults to "about:blank" per RFC 7807.
	Type string `json:"type,omitempty"`

	// Title is a short, human-readable summary of the problem type.
	// It should not change from occurrence to occurrence of the same
	// problem type.
	Title string `json:"title"`

	// Status is the HTTP status code generated by the origin server
	// for this occurrence.
	Status int `json:"status"`

	// Detail is a human-readable explanation specific to this
	// occurrence. It must be safe to show to an unprivileged caller.
	Detail string `json:"detail,omitempty"`

	// Instance is a URI reference that identifies the specific
	// occurrence of the problem; typically the request path plus the
	// assigned request ID.
	Instance string `json:"instance,omitempty"`

	// RequestID surfaces the gateway's request ID as a first-class
	// field so callers do not need to parse Instance to correlate
	// with audit logs. This is an RFC 7807 extension member.
	RequestID string `json:"request_id,omitempty"`
}

// ProblemContentType is the media type required by RFC 7807 §3 for
// problem documents. Handlers must set the Content-Type response
// header to this value when emitting a Problem.
const ProblemContentType = "application/problem+json"
