package apiv1

import "time"

// Model is one entry in a ModelsResponse, describing a model the
// upstream Ollama instance has pulled and is ready to serve. The
// shape follows Ollama's /api/tags contract verbatim minus the
// fields clients have not asked for yet (templates, license blobs,
// modelfile text). Breaking changes to this type require a /vN bump.
type Model struct {
	// Name is the fully-qualified model reference, e.g.
	// "qwen2.5:7b-instruct". Stable across Ollama restarts.
	Name string `json:"name"`

	// Digest is Ollama's content hash of the model blob. Useful
	// for operators validating an air-gap deployment against an
	// expected model set.
	Digest string `json:"digest,omitempty"`

	// Size is the on-disk size of the model in bytes.
	Size int64 `json:"size,omitempty"`

	// ModifiedAt is the last time the model was pulled or touched
	// locally. RFC 3339 formatted on the wire.
	ModifiedAt time.Time `json:"modified_at,omitempty"`

	// Family is Ollama's coarse architecture label (e.g. "qwen2",
	// "llama", "mistral"). Optional because some Ollama versions
	// omit it on older pulled models.
	Family string `json:"family,omitempty"`

	// ParameterSize is a human-readable size label like "7B".
	ParameterSize string `json:"parameter_size,omitempty"`

	// Quantization is the compression scheme in use, e.g. "Q4_K_M".
	Quantization string `json:"quantization,omitempty"`
}

// ModelsResponse is the body of GET /v1/models. Wrapped in an
// object (rather than a bare array) so we can add aggregate
// metadata — default model, bridge URL, etc. — without breaking
// the contract.
type ModelsResponse struct {
	// Models is the list of installed models. An empty list is
	// not an error; operators with a fresh Ollama volume see it
	// as a signal to `ollama pull`.
	Models []Model `json:"models"`

	// Default names the model the gateway will use when an
	// AskRequest omits its model field. Clients render this in
	// model pickers so the user knows which default applies.
	Default string `json:"default,omitempty"`
}

// Tool is one entry in a ToolsResponse, describing an MCP tool the
// bridge has registered from its configured MCP servers.
type Tool struct {
	// Name is the fully-qualified MCP tool name, e.g.
	// "opencost.allocation". Mirrors apiv1.ToolCall.Name so the
	// request-side and discovery-side use the same identifier.
	Name string `json:"name"`

	// Description is the tool's own human-readable description, as
	// advertised by the MCP server at registration time.
	Description string `json:"description,omitempty"`

	// Server optionally identifies the MCP server the tool came
	// from. Useful in multi-server deployments; can be empty in
	// v0.1 since we ship with a single server (OpenCost).
	Server string `json:"server,omitempty"`
}

// ToolsResponse is the body of GET /v1/tools.
//
// v0.1 LIMITATION: jonigl/ollama-mcp-bridge does not currently
// expose an endpoint for listing the MCP tools it has discovered
// from its configured servers. Until that is added upstream (or
// until the gateway learns to cache tools observed in chat
// responses), this endpoint returns an empty list with
// DiscoveryDeferred=true so clients can render "tool discovery not
// yet supported" rather than silently assume the bridge is
// misconfigured. See docs/architecture.md §10.
type ToolsResponse struct {
	// Tools is the list of known MCP tools; may be empty.
	Tools []Tool `json:"tools"`

	// DiscoveryDeferred signals that the empty list is a known
	// gap, not a cluster misconfiguration. Clients should treat
	// this as an informational flag, not an error.
	DiscoveryDeferred bool `json:"discovery_deferred,omitempty"`
}
