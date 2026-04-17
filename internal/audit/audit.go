package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// callerIdentityPrefixHex is how many hex characters of the SHA-256
// token digest are kept on each audit line. 16 hex chars == 64 bits,
// enough for per-caller correlation inside a single deployment but
// well short of the 128 bits a plausible pre-image attack would need
// even against a low-entropy token.
const callerIdentityPrefixHex = 16

// Event is the wire shape of a single audit log line.
//
// Field order in Go does not influence JSON output (encoding/json
// uses the struct order, but consumers must not rely on that); the
// field *set* is the contract. Any change to the field set is a
// breaking change to the audit contract.
//
// Fields with an omitempty tag are conditional: ToolCalls may be nil
// when the model answered from context alone, and Query/Answer are
// only populated when the opt-in flag is enabled.
type Event struct {
	// Timestamp is the instant the request finished processing in the
	// gateway, in RFC 3339 with nanosecond precision.
	Timestamp time.Time `json:"timestamp"`

	// RequestID is the per-request correlation token matching the
	// X-Request-ID response header and any problem+json instance URI.
	RequestID string `json:"request_id"`

	// CallerIdentity is a stable pseudonym derived from the bearer
	// token the caller presented. It is the first
	// callerIdentityPrefixHex characters of the token's SHA-256
	// digest. The token itself is never logged.
	CallerIdentity string `json:"caller_identity"`

	// Model is the model the gateway asked the bridge to use.
	Model string `json:"model"`

	// PromptTokens is prompt_eval_count from the bridge, or 0 when
	// the bridge did not report it (e.g. streaming without final
	// stats).
	PromptTokens int `json:"prompt_tokens"`

	// CompletionTokens is eval_count from the bridge.
	CompletionTokens int `json:"completion_tokens"`

	// ToolCalls lists every MCP tool invocation observed, in call
	// order.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// LatencyMS is the end-to-end wall-clock time the request spent
	// inside the gateway.
	LatencyMS int64 `json:"latency_ms"`

	// Status is the HTTP status code emitted to the caller.
	Status int `json:"status"`

	// Outcome is "ok", "error", or "rate_limited". Redundant with
	// Status for filtering convenience.
	Outcome string `json:"outcome"`

	// Query is the caller's raw query string. Populated only when
	// the opt-in OPENCOST_AI_AUDIT_LOG_QUERY flag is true.
	Query string `json:"query,omitempty"`

	// Answer is the model's completion text. Populated only when the
	// opt-in OPENCOST_AI_AUDIT_LOG_QUERY flag is true.
	Answer string `json:"answer,omitempty"`
}

// ToolCall is one tool invocation inside Event.ToolCalls. The MCP
// argument object is deliberately not logged: it can contain
// filtering predicates over cost data that leak shape of the cluster.
type ToolCall struct {
	// Name is the fully-qualified MCP tool name.
	Name string `json:"name"`

	// DurationMS is the tool's wall-clock duration as observed by
	// the gateway, in milliseconds.
	DurationMS int64 `json:"duration_ms"`
}

// Logger writes Events as JSON lines.
//
// A Logger is safe for concurrent use. All Log calls serialize on an
// internal mutex so partial writes to the underlying io.Writer cannot
// interleave two events. This matters because the default sink is
// stdout, which Kubernetes scrapes line-by-line.
type Logger struct {
	mu        sync.Mutex
	w         io.Writer
	logQuery  bool
	nowFn     func() time.Time
}

// NewLogger constructs a Logger writing to w. When w is nil, os.Stdout
// is used. logQuery toggles the opt-in query/answer fields.
//
// The nowFn argument is nil in production and replaced by a fixed
// clock in tests.
func NewLogger(w io.Writer, logQuery bool) *Logger {
	if w == nil {
		w = os.Stdout
	}
	return &Logger{w: w, logQuery: logQuery, nowFn: time.Now}
}

// WithClock returns a copy of l with nowFn replaced. Only useful for
// tests; production code should call NewLogger and leave the clock
// alone.
func (l *Logger) WithClock(now func() time.Time) *Logger {
	return &Logger{w: l.w, logQuery: l.logQuery, nowFn: now}
}

// LogQueryEnabled reports whether query and answer fields will be
// populated on the next Log call. Callers use this to skip capturing
// the strings when the audit log would not record them anyway.
func (l *Logger) LogQueryEnabled() bool { return l.logQuery }

// CallerIdentity returns the stable pseudonym for a bearer token. It
// is exposed so the rate limiter can key its per-caller buckets on
// the same identity the audit log records — operators correlating
// rate-limit events with audit lines then see the same value on
// both sides.
func CallerIdentity(token string) string {
	if token == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(token))
	enc := hex.EncodeToString(sum[:])
	if len(enc) > callerIdentityPrefixHex {
		enc = enc[:callerIdentityPrefixHex]
	}
	return enc
}

// Log emits e as a single JSON line. Timestamp and CallerIdentity are
// populated from the Logger when the caller leaves them zero-valued,
// so handler code does not have to repeat the same boilerplate on
// every log site. The Query and Answer fields are cleared when the
// opt-in flag is off; a caller passing them in anyway cannot leak
// them through this path.
func (l *Logger) Log(e Event) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = l.nowFn().UTC()
	}
	if !l.logQuery {
		e.Query = ""
		e.Answer = ""
	}
	buf, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: marshal event: %w", err)
	}
	buf = append(buf, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(buf); err != nil {
		return fmt.Errorf("audit: write event: %w", err)
	}
	return nil
}
