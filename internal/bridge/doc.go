// Package bridge is the HTTP client for jonigl/ollama-mcp-bridge.
//
// The bridge speaks a superset of the Ollama API: POST /api/chat is
// intercepted and rewritten to inject pre-registered MCP tools and
// execute any tool calls the model emits; every other path (/api/tags,
// /api/show, /api/ps, ...) is transparently proxied to the underlying
// Ollama daemon. The gateway therefore talks to exactly one network
// endpoint regardless of whether the operation is MCP-aware.
//
// Per CLAUDE.md this is the only gateway package that imports net/http
// for upstream calls. All other packages stay transport-agnostic so
// that swapping the bridge out (for a native Go MCP client in v0.3)
// touches just this directory.
//
// The client is intentionally narrow: Chat and Models are what the v1
// HTTP handlers need. Streaming chat and the MCP tool-discovery path
// are deferred to later sessions — see the TODO comments next to each
// method.
package bridge
