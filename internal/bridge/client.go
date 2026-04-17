package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultHTTPTimeout is the per-call upper bound applied when the
// caller does not supply their own ctx deadline. It matches
// config.DefaultRequestTimeout so a misconfigured ctx still honours
// the documented envelope.
const DefaultHTTPTimeout = 120 * time.Second

// maxErrorBodyBytes caps how much of an upstream error body we
// retain inside an *Error. Large enough to capture a FastAPI detail
// blob, small enough that a rogue upstream cannot blow up memory.
const maxErrorBodyBytes = 4 << 10

// Client calls a jonigl/ollama-mcp-bridge instance. It is safe for
// concurrent use. The zero value is not usable; construct with New.
type Client struct {
	baseURL *url.URL
	hc      *http.Client
	// streamHC is the client used for streaming endpoints. It shares
	// hc's Transport (so connection pooling and any operator-supplied
	// middleware apply uniformly) but carries Timeout=0 because
	// http.Client.Timeout bounds the whole response, including body
	// reads — a non-zero value would kill a healthy long-lived SSE
	// stream at the first timeout tick. Streaming callers rely on
	// ctx cancellation for liveness instead.
	streamHC *http.Client
	ua       string
}

// Option configures a Client at construction time. The exported
// option surface is intentionally small — WithHTTPClient and
// WithUserAgent only — so the internals can evolve without breaking
// callers. New options land here when a concrete caller needs them,
// not speculatively.
type Option func(*Client)

// WithHTTPClient overrides the http.Client used for upstream calls.
// The supplied client's Timeout still applies on top of any ctx
// deadline the caller sets. Intended for tests and for operators who
// want to thread custom TLS config or tracing middleware. A nil
// client is ignored so a misconfigured caller cannot panic the
// process on the first Do — the default client stays in place.
//
// A separate streaming client is derived from hc's Transport with
// Timeout=0 so streaming endpoints do not inherit the per-request
// body timeout. If hc has no Transport, streaming falls back to the
// default transport.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc == nil {
			return
		}
		c.hc = hc
		c.streamHC = &http.Client{Transport: hc.Transport}
	}
}

// WithUserAgent overrides the User-Agent sent with every request.
// Defaults to "opencost-ai-gateway/dev".
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.ua = ua }
}

// New returns a Client rooted at baseURL. baseURL must have an
// http or https scheme and a non-empty host; anything else is a
// construction-time error so misconfiguration surfaces at wire-up
// rather than on the first request.
func New(baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse bridge URL %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("bridge URL %q: scheme must be http or https", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("bridge URL %q: missing host", baseURL)
	}
	// Normalise the base path so joining with a relative endpoint
	// like "api/chat" produces <base>/api/chat regardless of whether
	// the operator's config ended with a trailing slash. Using
	// url.Parse against a base without a trailing slash drops the
	// last path segment (RFC 3986 §5.3), which would strip the
	// "/bridge" prefix from "http://host/bridge" — flagged by
	// Copilot on PR #5.
	u.Path = strings.TrimRight(u.Path, "/")

	c := &Client{
		baseURL: u,
		hc:      &http.Client{Timeout: DefaultHTTPTimeout},
		// No Timeout on the streaming client — see streamHC doc.
		streamHC: &http.Client{},
		ua:       "opencost-ai-gateway/dev",
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// BaseURL returns the normalised base URL. Mostly useful for logs.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Chat performs a non-streaming chat completion. The bridge
// intercepts this path to inject MCP tools and round-trip their
// calls before returning the final assistant message, so a caller
// receives a single response with all tool_calls already resolved.
//
// Chat forces req.Stream to false; the streaming SSE variant lands
// in a later session. ctx must be non-nil; callers that have no
// deadline should pass context.Background explicitly.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// Forcing stream=false keeps the response a single JSON object
	// instead of a newline-delimited stream — the latter would
	// break json.Decoder below.
	req.Stream = false

	var resp ChatResponse
	if err := c.do(ctx, http.MethodPost, "/api/chat", req, &resp, "chat"); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Models returns the list of models Ollama reports through the
// bridge's /api/tags proxy. An empty list is not an error — a
// freshly installed Ollama has no models pulled yet, and callers
// should render that as "no models installed" rather than a failure.
// ctx must be non-nil.
func (c *Client) Models(ctx context.Context) ([]TagModel, error) {
	var env tagsResponse
	if err := c.do(ctx, http.MethodGet, "/api/tags", nil, &env, "models"); err != nil {
		return nil, err
	}
	return env.Models, nil
}

// ChatStream opens a streaming POST /api/chat against the bridge and
// returns a ChatStream the caller iterates with Next. req.Stream is
// forced to true; the non-streaming Chat method is still the right
// call when the caller does not need intermediate events.
//
// The returned stream holds an open HTTP connection; callers must
// call Close when done, even if Next returned an error — the
// deferred Close pattern is what keeps the underlying response body
// from leaking a goroutine inside net/http.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest) (*ChatStream, error) {
	req.Stream = true

	reqURL := *c.baseURL
	reqURL.Path = reqURL.Path + "/api/chat"
	reqURL.RawPath = ""

	buf, err := json.Marshal(req)
	if err != nil {
		return nil, &Error{Op: "chat_stream", Err: fmt.Errorf("marshal request: %w", err)}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), bytes.NewReader(buf))
	if err != nil {
		return nil, &Error{Op: "chat_stream", Err: fmt.Errorf("build request: %w", err)}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/x-ndjson, application/json")
	httpReq.Header.Set("User-Agent", c.ua)

	httpResp, err := c.streamHC.Do(httpReq)
	if err != nil {
		return nil, &Error{Op: "chat_stream", Err: err}
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBodyBytes))
		httpResp.Body.Close()
		return nil, &Error{
			Op:     "chat_stream",
			Status: httpResp.StatusCode,
			Body:   strings.TrimSpace(string(snippet)),
		}
	}
	return newChatStream(httpResp.Body), nil
}

// do is the shared request plumbing. It JSON-encodes body when
// non-nil, sets Content-Type / Accept / User-Agent, executes the
// request, and decodes a successful response into out. Non-2xx
// responses become *Error; transport failures become *Error with
// Err populated and Status == 0.
func (c *Client) do(ctx context.Context, method, path string, body, out any, op string) error {
	// Build a copy of baseURL with the endpoint appended to its
	// path. Going through (*url.URL).Parse would fold RFC 3986
	// reference resolution over the path, which drops the final
	// segment of a prefix like "/bridge" — not what we want.
	reqURL := *c.baseURL
	reqURL.Path = reqURL.Path + "/" + strings.TrimLeft(path, "/")
	reqURL.RawPath = ""

	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return &Error{Op: op, Err: fmt.Errorf("marshal request: %w", err)}
		}
		reqBody = bytes.NewReader(buf)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, reqURL.String(), reqBody)
	if err != nil {
		return &Error{Op: op, Err: fmt.Errorf("build request: %w", err)}
	}
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", c.ua)

	httpResp, err := c.hc.Do(httpReq)
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Drain a bounded prefix of the body so *Error carries
		// something operators can read without exposing the whole
		// upstream payload on memory pressure.
		snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, maxErrorBodyBytes))
		return &Error{
			Op:     op,
			Status: httpResp.StatusCode,
			Body:   strings.TrimSpace(string(snippet)),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(httpResp.Body).Decode(out); err != nil {
		return &Error{Op: op, Err: fmt.Errorf("decode response: %w", err)}
	}
	return nil
}
