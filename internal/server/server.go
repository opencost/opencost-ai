package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/opencost/opencost-ai/internal/auth"
	"github.com/opencost/opencost-ai/internal/bridge"
	"github.com/opencost/opencost-ai/pkg/apiv1"
)

// Bridge is the subset of *bridge.Client the handlers need. Keeping
// it an interface lets tests swap in a fake without spinning up an
// httptest.Server in every test case.
type Bridge interface {
	Chat(ctx context.Context, req bridge.ChatRequest) (*bridge.ChatResponse, error)
	Models(ctx context.Context) ([]bridge.TagModel, error)
}

// Options bundles the wire-up parameters cmd/gateway needs to hand
// in. A zero-value Options is invalid; construct one explicitly and
// pass it to New.
type Options struct {
	// Bridge is the client used to reach ollama-mcp-bridge.
	Bridge Bridge

	// AuthValidator is checked on every authenticated endpoint. In
	// production this is *auth.Source; tests may substitute a stub.
	AuthValidator auth.Validator

	// DefaultModel is substituted when an AskRequest omits model.
	DefaultModel string

	// MaxRequestBytes is the per-request body ceiling. The POST
	// /v1/ask handler rejects larger envelopes before decoding, so
	// a pathological client cannot OOM the process.
	MaxRequestBytes int64

	// Logger is used for structured logs. Defaults to slog.Default
	// when nil.
	Logger *slog.Logger
}

// New returns an http.Handler exposing the v1 endpoint tree. The
// returned handler is safe for use in http.Server.Handler.
//
// Unauthenticated routes are not installed here — /v1/health is
// cmd/gateway's responsibility so liveness probes keep working even
// if wire-up is incomplete. Everything this package exposes is
// guarded by the bearer-token middleware.
func New(opts Options) (http.Handler, error) {
	if opts.Bridge == nil {
		return nil, errors.New("server: Bridge is required")
	}
	if opts.AuthValidator == nil {
		return nil, errors.New("server: AuthValidator is required")
	}
	if opts.DefaultModel == "" {
		return nil, errors.New("server: DefaultModel is required")
	}
	if opts.MaxRequestBytes <= 0 {
		return nil, errors.New("server: MaxRequestBytes must be positive")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	h := &handlers{
		bridge:          opts.Bridge,
		logger:          opts.Logger,
		defaultModel:    opts.DefaultModel,
		maxRequestBytes: opts.MaxRequestBytes,
	}

	authMW := auth.Middleware(opts.AuthValidator, opts.Logger)
	ridMW := requestIDMiddleware()

	mux := http.NewServeMux()
	mux.Handle("POST /v1/ask", ridMW(authMW(http.HandlerFunc(h.ask))))
	mux.Handle("GET /v1/tools", ridMW(authMW(http.HandlerFunc(h.tools))))
	mux.Handle("GET /v1/models", ridMW(authMW(http.HandlerFunc(h.models))))
	return mux, nil
}

// mapBridgeError converts a bridge.Error into a caller-safe
// problem+json. Non-bridge errors collapse to 502 because the only
// place in the handler chain that returns them is the upstream call
// path; anything else is a programming error and should have been
// caught earlier.
func mapBridgeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, op string, err error) {
	var bErr *bridge.Error
	if errors.As(err, &bErr) {
		// Transport-level failure (DNS, conn refused, ctx cancel).
		if bErr.Status == 0 {
			logger.Error("bridge transport failure",
				"op", op, "request_id", requestIDFromContext(r.Context()), "err", err)
			writeProblem(w, r, http.StatusBadGateway,
				"Bad Gateway",
				"upstream bridge is unreachable")
			return
		}
		// Propagate a handful of well-known status codes with
		// matching problem titles; everything else collapses to
		// 502 so we do not accidentally leak upstream framing.
		switch bErr.Status {
		case http.StatusNotFound:
			logger.Warn("bridge 404",
				"op", op, "request_id", requestIDFromContext(r.Context()), "body", bErr.Body)
			writeProblem(w, r, http.StatusBadGateway,
				"Bad Gateway",
				"requested upstream resource not found")
		case http.StatusServiceUnavailable:
			logger.Warn("bridge 503",
				"op", op, "request_id", requestIDFromContext(r.Context()), "body", bErr.Body)
			writeProblem(w, r, http.StatusServiceUnavailable,
				"Service Unavailable",
				"upstream bridge is not ready")
		default:
			logger.Error("bridge non-2xx",
				"op", op, "status", bErr.Status, "request_id", requestIDFromContext(r.Context()), "body", bErr.Body)
			writeProblem(w, r, http.StatusBadGateway,
				"Bad Gateway",
				"upstream bridge returned an error")
		}
		return
	}
	logger.Error("bridge unknown failure",
		"op", op, "request_id", requestIDFromContext(r.Context()), "err", err)
	writeProblem(w, r, http.StatusBadGateway,
		"Bad Gateway",
		"upstream bridge returned an error")
}

// problemTitles map common statuses to their canonical titles. Kept
// as a lookup table so handlers don't hard-code strings inline.
//
// The table intentionally does not include 401/503 — those come from
// the auth middleware which composes its own titles.
var problemTitles = map[int]string{
	http.StatusBadRequest:          "Bad Request",
	http.StatusUnsupportedMediaType: "Unsupported Media Type",
	http.StatusRequestEntityTooLarge: "Payload Too Large",
	http.StatusInternalServerError: "Internal Server Error",
	http.StatusBadGateway:          "Bad Gateway",
}

// problemTitleFor looks up the canonical title for status or falls
// back to http.StatusText when the table does not have an entry.
func problemTitleFor(status int) string {
	if t, ok := problemTitles[status]; ok {
		return t
	}
	return http.StatusText(status)
}

// assertApiv1Compile keeps the apiv1 import load-bearing so gopls
// does not warn when handlers.go is edited independently of this
// file. Trivial at runtime, zero cost at build time.
var _ = apiv1.ProblemContentType
