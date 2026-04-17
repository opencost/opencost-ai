package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opencost/opencost-ai/internal/bridge"
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
}

// ask implements POST /v1/ask, non-streaming. Streaming SSE lands in
// a follow-up session per the delivery plan in architecture.md §9.
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
			"request body is not valid JSON")
		return
	}
	if err := validateAskRequest(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest,
			problemTitleFor(http.StatusBadRequest), err.Error())
		return
	}
	if req.Stream {
		// The task scope is non-streaming only. Reject rather than
		// silently degrade so clients that set Stream:true know
		// they're talking to the non-streaming build.
		writeProblem(w, r, http.StatusBadRequest,
			problemTitleFor(http.StatusBadRequest),
			"streaming responses are not yet supported by this gateway")
		return
	}

	model := req.Model
	if model == "" {
		model = h.defaultModel
	}

	reqID := requestIDFromContext(r.Context())
	start := time.Now()
	resp, err := h.bridge.Chat(r.Context(), bridge.ChatRequest{
		Model: model,
		Messages: []bridge.Message{
			{Role: "user", Content: req.Query},
		},
	})
	if err != nil {
		mapBridgeError(w, r, h.logger, "chat", err)
		return
	}

	out := apiv1.AskResponse{
		RequestID: reqID,
		Model:     resp.Model,
		Query:     req.Query,
		Answer:    resp.Message.Content,
		ToolCalls: toAPIToolCalls(resp.Message.ToolCalls),
		Usage: apiv1.Usage{
			PromptTokens:     resp.PromptEvalCount,
			CompletionTokens: resp.EvalCount,
		},
		LatencyMS: time.Since(start).Milliseconds(),
	}
	writeJSON(w, r, http.StatusOK, out)
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

// discardBody drains and closes r.Body. Not every handler needs it,
// but the ask handler does so the connection stays reusable after
// an early-return error.
//
//nolint:unused // retained for the streaming path; called in tests.
func discardBody(r *http.Request) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
}
