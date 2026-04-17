package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opencost/opencost-ai/internal/audit"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/internal/metrics"
	"github.com/opencost/opencost-ai/internal/ratelimit"
	"github.com/opencost/opencost-ai/internal/requestid"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// maxQueryBytes enforces the 4 KiB per-query ceiling documented in
// docs/architecture.md §7.2. It is separate from MaxRequestBytes
// (which bounds the JSON envelope) so a long query can't be hidden
// inside large metadata.
const maxQueryBytes = 4 * 1024

type handlers struct {
	bridge          Bridge
	logger          *slog.Logger
	defaultModel    string
	maxRequestBytes int64
	audit           *audit.Logger
	limiter         *ratelimit.Limiter
	metrics         *metrics.Registry
}

// ask implements POST /v1/ask. When req.Stream is false it returns a
// single JSON body; when true it upgrades the response to Server-Sent
// Events and emits typed events (`thinking`, `tool_call`,
// `tool_result`, `token`, `done`) per docs/architecture.md §7.2.
func (h *handlers) ask(w http.ResponseWriter, r *http.Request) {
	if err := requireJSON(r); err != nil {
		writeProblem(w, r, http.StatusUnsupportedMediaType,
			problemTitleFor(http.StatusUnsupportedMediaType), err.Error())
		return
	}

	// Enforce the envelope ceiling *before* decoding so a 50 MB
	// blob cannot exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBytes)

	var req apiv1.AskRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge,
				problemTitleFor(http.StatusRequestEntityTooLarge),
				"request body exceeds configured limit")
			return
		}
		writeProblem(w, r, http.StatusBadRequest,
			problemTitleFor(http.StatusBadRequest),
			decodeErrorDetail(err))
		return
	}
	// A single JSON object is the entire contract — no trailing
	// tokens, no second value. Draining one more token and requiring
	// io.EOF closes the "valid JSON followed by garbage" hole that
	// DisallowUnknownFields alone does not cover.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest,
			problemTitleFor(http.StatusBadRequest),
			"request body must contain exactly one JSON object")
		return
	}
	if err := validateAskRequest(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest,
			problemTitleFor(http.StatusBadRequest), err.Error())
		return
	}

	// Extract the bearer token fresh here so CallerIdentity stays a
	// property of the authenticated principal — not of some earlier
	// parsed-and-discarded header. The auth middleware already
	// validated it, so failure at this layer is impossible unless a
	// future refactor breaks the chain.
	token, _ := parseBearerToken(r.Header.Get("Authorization"))
	caller := audit.CallerIdentity(token)

	if !h.limiter.Allow(caller) {
		h.metrics.RateLimited().Inc()
		reqID := requestid.FromContext(r.Context())
		detail := fmt.Sprintf("rate limit of %d requests per minute exceeded", h.limiter.PerMinute())
		h.logAudit(audit.Event{
			RequestID:      reqID,
			CallerIdentity: caller,
			Model:          modelOrDefault(req.Model, h.defaultModel),
			Status:         http.StatusTooManyRequests,
			Outcome:        "rate_limited",
			Query:          req.Query,
		})
		writeProblem(w, r, http.StatusTooManyRequests,
			problemTitleFor(http.StatusTooManyRequests), detail)
		return
	}

	model := modelOrDefault(req.Model, h.defaultModel)
	reqID := requestid.FromContext(r.Context())
	bridgeReq := bridge.ChatRequest{
		Model: model,
		Messages: []bridge.Message{
			{Role: "user", Content: req.Query},
		},
	}

	if req.Stream {
		h.askStream(w, r, req, bridgeReq, caller, reqID)
		return
	}

	start := time.Now()
	resp, err := h.bridge.Chat(r.Context(), bridgeReq)
	if err != nil {
		h.recordUpstreamError("chat", err)
		mapBridgeError(w, r, h.logger, "chat", err)
		return
	}
	latency := time.Since(start)

	tools := toAPIToolCalls(resp.Message.ToolCalls)
	h.observeTokens(resp.Model, resp.PromptEvalCount, resp.EvalCount)
	h.observeToolCalls(tools)

	out := apiv1.AskResponse{
		RequestID: reqID,
		Model:     resp.Model,
		Query:     req.Query,
		Answer:    resp.Message.Content,
		ToolCalls: tools,
		Usage: apiv1.Usage{
			PromptTokens:     resp.PromptEvalCount,
			CompletionTokens: resp.EvalCount,
		},
		LatencyMS: latency.Milliseconds(),
	}
	h.logAudit(audit.Event{
		RequestID:        reqID,
		CallerIdentity:   caller,
		Model:            resp.Model,
		PromptTokens:     resp.PromptEvalCount,
		CompletionTokens: resp.EvalCount,
		ToolCalls:        toAuditToolCalls(tools),
		LatencyMS:        latency.Milliseconds(),
		Status:           http.StatusOK,
		Outcome:          "ok",
		Query:            req.Query,
		Answer:           resp.Message.Content,
	})
	writeJSON(w, r, http.StatusOK, out)
}

// modelOrDefault resolves the model string for bridge dispatch and
// for audit/metric labeling so the two stay consistent.
func modelOrDefault(requested, fallback string) string {
	if requested == "" {
		return fallback
	}
	return requested
}

// parseBearerToken is a tolerant re-extractor of the Authorization
// header. It intentionally duplicates the subset of internal/auth's
// logic that handlers need, because pulling auth.extractBearer out
// of that package is a wider refactor than the current scope.
func parseBearerToken(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		return "", false
	}
	if !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}

// logAudit centralises the "never block the response on an audit
// failure" policy. A write error is logged at Error level so
// operators notice, but is not surfaced to the caller.
func (h *handlers) logAudit(e audit.Event) {
	if err := h.audit.Log(e); err != nil {
		h.logger.Error("audit log write failed",
			"request_id", e.RequestID, "err", err)
	}
}

// observeTokens records per-model prompt and completion token totals
// on the metrics registry. Called from both the streaming and non-
// streaming paths so the series looks the same regardless of mode.
func (h *handlers) observeTokens(model string, prompt, completion int) {
	if prompt > 0 {
		h.metrics.ModelTokens().WithLabelValues(model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		h.metrics.ModelTokens().WithLabelValues(model, "completion").Add(float64(completion))
	}
}

// observeToolCalls increments the per-tool counter and, if the bridge
// ever surfaces per-call timing, records it in the duration histogram.
// Today neither the streaming nor the non-streaming bridge response
// carries a per-tool duration, so the DurationMS>0 branch is dormant
// and the histogram stays pre-registered but unobserved. The guard
// stays so that when the bridge grows a timing field the code lights
// up without a second metric-registry wiring change.
func (h *handlers) observeToolCalls(tcs []apiv1.ToolCall) {
	for _, tc := range tcs {
		h.metrics.ToolCalls().WithLabelValues(tc.Name).Inc()
		if tc.DurationMS > 0 {
			h.metrics.ToolDuration().WithLabelValues(tc.Name).
				Observe(float64(tc.DurationMS) / 1000.0)
		}
	}
}

// recordUpstreamError translates a bridge failure into the
// upstream_errors_total series. The "kind" label keeps the
// transport-vs-HTTP distinction that mapBridgeError makes for the
// caller-facing problem+json.
func (h *handlers) recordUpstreamError(op string, err error) {
	kind := "unknown"
	var bErr *bridge.Error
	if errors.As(err, &bErr) {
		switch {
		case bErr.Status == 0:
			kind = "transport"
		case bErr.Status >= 500:
			kind = "http_5xx"
		case bErr.Status >= 400:
			kind = "http_4xx"
		default:
			kind = "http_other"
		}
	}
	h.metrics.UpstreamErrors().WithLabelValues(op, kind).Inc()
}

// toAuditToolCalls translates the wire-facing tool-call shape into
// the narrower audit representation (arguments dropped by design).
func toAuditToolCalls(in []apiv1.ToolCall) []audit.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]audit.ToolCall, 0, len(in))
	for _, tc := range in {
		out = append(out, audit.ToolCall{Name: tc.Name, DurationMS: tc.DurationMS})
	}
	return out
}

// tools implements GET /v1/tools. v0.1 returns an empty list with
// DiscoveryDeferred=true because jonigl/ollama-mcp-bridge does not
// yet expose an MCP tool listing endpoint — see the doc comment on
// apiv1.ToolsResponse.
func (h *handlers) tools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, apiv1.ToolsResponse{
		Tools:             []apiv1.Tool{},
		DiscoveryDeferred: true,
	})
}

// models implements GET /v1/models. Delegates to bridge.Models and
// reshapes the Ollama-shaped TagModel into the thinner apiv1.Model.
func (h *handlers) models(w http.ResponseWriter, r *http.Request) {
	tags, err := h.bridge.Models(r.Context())
	if err != nil {
		mapBridgeError(w, r, h.logger, "models", err)
		return
	}
	out := apiv1.ModelsResponse{
		Models:  make([]apiv1.Model, 0, len(tags)),
		Default: h.defaultModel,
	}
	for _, m := range tags {
		out.Models = append(out.Models, apiv1.Model{
			Name:          m.Name,
			Digest:        m.Digest,
			Size:          m.Size,
			ModifiedAt:    m.ModifiedAt,
			Family:        m.Details.Family,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.QuantizationLevel,
		})
	}
	writeJSON(w, r, http.StatusOK, out)
}

// decodeErrorDetail returns a caller-safe problem+json detail string
// for a json.Decoder error. It distinguishes three cases operators
// hit in the wild: unknown-field rejections from DisallowUnknownFields
// (useful for SDK authors debugging a schema mismatch), structural
// JSON syntax errors, and wrong-type-for-field errors. Anything else
// falls back to the generic "not valid JSON" message — we intentionally
// do not surface err.Error() because it can include caller payload
// fragments that the audit log should not leak.
//
// DisallowUnknownFields does not expose a typed error in stdlib (it is
// a plain fmt.Errorf("json: unknown field %q", key) inside
// encoding/json), so the prefix check below is the supported detection
// pattern. It is stable across Go releases precisely because changing
// it would silently break every downstream user that relies on it.
func decodeErrorDetail(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		// Preserve the quoted field name the stdlib already produced
		// so the caller sees "unknown field \"foo\"" without us having
		// to re-parse. Strip the "json: " prefix for a cleaner wire.
		return "request body has " + strings.TrimPrefix(msg, "json: ")
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		if typeErr.Field != "" {
			return "request body field " + typeErr.Field + " has wrong type"
		}
		return "request body has a field of the wrong type"
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return "request body is not valid JSON"
	}
	return "request body is not valid JSON"
}

// requireJSON returns a caller-safe error if Content-Type is not
// application/json. Charset parameters are tolerated per RFC 8259.
func requireJSON(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return errors.New("Content-Type must be application/json")
	}
	media := ct
	if idx := strings.Index(ct, ";"); idx >= 0 {
		media = ct[:idx]
	}
	media = strings.TrimSpace(strings.ToLower(media))
	if media != "application/json" {
		return errors.New("Content-Type must be application/json")
	}
	return nil
}

// validateAskRequest enforces the field-level contract from
// docs/architecture.md §7.2. Errors returned are safe to surface in
// problem+json — they describe structural requirements, not values.
func validateAskRequest(r *apiv1.AskRequest) error {
	q := strings.TrimSpace(r.Query)
	if q == "" {
		return errors.New("query is required")
	}
	if len(r.Query) > maxQueryBytes {
		return errors.New("query exceeds maximum length")
	}
	if !utf8.ValidString(r.Query) {
		return errors.New("query must be valid UTF-8")
	}
	if r.ConversationID != "" && !isUUIDLike(r.ConversationID) {
		return errors.New("conversation_id must be a UUID")
	}
	return nil
}

// isUUIDLike is a cheap structural check for 8-4-4-4-12 hex, which
// is all the handler needs to reject junk. Full RFC 4122 version /
// variant enforcement belongs at the conversation store, not here.
func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

// toAPIToolCalls translates the bridge-level tool call shape into
// the apiv1 shape. Duration is not known at this layer (the bridge
// executes tools internally and does not report per-call timing in
// the non-streaming response), so DurationMS is left zero.
func toAPIToolCalls(in []bridge.ToolCall) []apiv1.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]apiv1.ToolCall, 0, len(in))
	for _, tc := range in {
		out = append(out, apiv1.ToolCall{
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	return out
}

// writeJSON serialises v and writes it with status. Ignores errors
// on Write because by that point the status has been flushed and
// we can do nothing useful with a mid-body I/O failure.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError,
			problemTitleFor(http.StatusInternalServerError),
			"failed to encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

