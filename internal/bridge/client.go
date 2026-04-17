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
	ua      string
}

// Option configures a Client at construction time. Options are kept
// internal — the only currently exported one is WithHTTPClient — to
// leave room to evolve without breaking callers.
type Option func(*Client)

// WithHTTPClient overrides the http.Client used for upstream calls.
// The supplied client's Timeout still applies on top of any ctx
// deadline the caller sets. Intended for tests and for operators who
// want to thread custom TLS config or tracing middleware.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.hc = hc }
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
	// Normalise away trailing slash so ResolveReference composes
	// predictably regardless of whether the operator's config has
	// a trailing slash or not.
	u.Path = strings.TrimRight(u.Path, "/")

	c := &Client{
		baseURL: u,
		hc:      &http.Client{Timeout: DefaultHTTPTimeout},
		ua:      "opencost-ai-gateway/dev",
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

// do is the shared request plumbing. It JSON-encodes body when
// non-nil, sets Content-Type / Accept / User-Agent, executes the
// request, and decodes a successful response into out. Non-2xx
// responses become *Error; transport failures become *Error with
// Err populated and Status == 0.
func (c *Client) do(ctx context.Context, method, path string, body, out any, op string) error {
	reqURL, err := c.baseURL.Parse(strings.TrimLeft(path, "/"))
	if err != nil {
		return &Error{Op: op, Err: fmt.Errorf("build URL: %w", err)}
	}

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
