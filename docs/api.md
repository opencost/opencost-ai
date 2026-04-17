# API reference — opencost-ai-gateway `/v1`

Operator-facing reference for the HTTP surface the gateway exposes.
This is the stable v0.1 contract: breaking changes require a `/v2`
bump. For intent and architectural rationale, see
`docs/architecture.md` (§7 for the original specification, §11 for
the delta between the spec and what actually shipped in v0.1.0).

**Status:** v0.1.0. Routes and field names below are frozen under
semver for the v0.x line. Additive fields on responses are not
breaking; clients MUST tolerate unknown fields. Removals or renames
wait for v1.0.

## 1. Transport and media types

- Base URL: `http://<gateway>:8080` (cluster `ClusterIP` by default;
  the Helm chart binds `service.port` to 8080).
- Content-Type for requests with a body: `application/json` (a
  `charset=utf-8` parameter is tolerated). Anything else is rejected
  with `415 Unsupported Media Type`.
- Content-Type for success responses: `application/json` on
  non-streaming paths; `text/event-stream` on streaming `POST /v1/ask`
  when `stream:true`.
- Content-Type for error responses: `application/problem+json`
  (RFC 7807). **No endpoint ever returns raw exception strings.**
- Every request/response carries `X-Request-ID`. Gateway-generated
  IDs are 16 lowercase hex chars. Callers may supply their own ID;
  the gateway validates it (printable ASCII subset, ≤128 bytes,
  per `internal/requestid.validCallerID`) and either echoes it as
  supplied or replaces it with a fresh 16-lowercase-hex value when
  validation fails. The resulting ID appears in the response
  header, the audit log, and the `instance` + `request_id` fields
  of any problem+json.

## 2. Authentication

Every route under `/v1/` **except `/v1/health`** requires a bearer
token:

```
Authorization: Bearer <token>
```

- Token is a static string loaded from the Kubernetes Secret mounted
  at `OPENCOST_AI_AUTH_TOKEN_FILE` (default
  `/var/run/secrets/opencost-ai/token`). Rotate by updating the
  Secret; the gateway picks up the new value on the next request
  after the file's mtime advances.
- Comparison is constant-time on equal-length inputs. Callers SHOULD
  provision a high-entropy token (≥ 32 bytes of random data; the
  chart documents `openssl rand -hex 32`).
- `401 Unauthorized` is returned with a `WWW-Authenticate: Bearer
  realm="opencost-ai"` header when the header is missing,
  malformed, has the wrong scheme, or carries an unrecognised
  token.
- `503 Service Unavailable` is returned when the gateway is up but
  has no token configured on disk — this is an operator bug, not a
  caller bug, and the 503 encourages a load balancer to back off
  rather than retry-storm.
- `GET /v1/health` intentionally bypasses auth so Kubernetes
  liveness probes keep working when the token file is missing or
  unreadable.

## 3. Request size and validation

The envelope ceiling is `OPENCOST_AI_MAX_REQUEST_BYTES` (default
8192 bytes) and applies to the entire JSON body. `AskRequest.query`
has its own 4 KiB ceiling enforced after parsing — a large envelope
cannot hide a large query behind bulky metadata.

Request bodies are decoded with `DisallowUnknownFields`; an
unrecognised top-level key returns `400 Bad Request` with a detail
naming the field. Every `POST /v1/ask` body must contain exactly
one JSON object — trailing tokens or a second value are rejected as
`400`.

All validation errors return problem+json with a
caller-safe `detail` string. The gateway never echoes raw caller
payload fragments back into error documents.

## 4. Endpoints

### 4.1 `GET /v1/health`

Liveness only. Returns `200` while the process is up and the HTTP
listener is accepting connections. **Does not probe upstream
dependencies.** Kubernetes readiness probes MUST NOT target this
endpoint until `/v1/ready` lands (see `docs/architecture.md`
§11.6).

- Auth: none.
- Request: no body.
- Response `200` body (`application/json`):

```json
{"status": "ok", "version": "0.1.0"}
```

`status` is `"ok"` in v0.1. Callers must treat any unknown value
as degraded so a future `"degraded"` from a readiness-aware
implementation does not read as "healthy".

### 4.2 `POST /v1/ask`

The main endpoint. Sends a natural-language query to the bridge,
which orchestrates MCP tool calls against the OpenCost MCP server
and returns a model-generated answer.

- Auth: required.
- Rate-limited: yes, per-caller (see §5).
- Content-Type: `application/json`.
- Methods other than `POST`: `405 Method Not Allowed`.

Request body (`apiv1.AskRequest`):

```json
{
  "query": "what did the platform namespace spend yesterday?",
  "model": "qwen2.5:7b-instruct",
  "stream": false,
  "conversation_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

| Field             | Type    | Required | Notes                                                   |
|-------------------|---------|----------|---------------------------------------------------------|
| `query`           | string  | yes      | Non-empty after trim, ≤ 4 KiB, valid UTF-8.             |
| `model`           | string  | no       | Defaults to `OPENCOST_AI_DEFAULT_MODEL`.                |
| `stream`          | bool    | no       | `false` (single JSON response) or `true` (SSE).         |
| `conversation_id` | string  | no       | When present, must be a 36-char UUID-shaped hex string. |

The `format` field from `docs/architecture.md` §7.2 is
intentionally absent in v0.1; see §11.4 of the architecture doc.

#### 4.2.1 Non-streaming response (`stream:false`)

`200 OK`, `application/json`, `apiv1.AskResponse`:

```json
{
  "request_id": "0a1b2c3d4e5f6789",
  "model": "qwen2.5:7b-instruct",
  "query": "what did the platform namespace spend yesterday?",
  "answer": "Yesterday the `platform` namespace spent **$42.17** …",
  "tool_calls": [
    {"name": "opencost.allocation", "args": {"window": "yesterday"}, "duration_ms": 0}
  ],
  "usage": {"prompt_tokens": 412, "completion_tokens": 187},
  "latency_ms": 1843
}
```

Notes on specific fields:

- `answer` is markdown formatted for terminal and web readability.
- `tool_calls[].duration_ms` is 0 in v0.1 — the bridge does not
  report per-call timing on the non-streaming path. The field is
  present for forward compatibility.
- `usage.prompt_tokens` / `usage.completion_tokens` are the
  bridge's `prompt_eval_count` / `eval_count` respectively. They
  may be zero when the upstream omits them.
- `latency_ms` is end-to-end gateway wall-clock time.

#### 4.2.2 Streaming response (`stream:true`)

`200 OK`, `text/event-stream`. Frames follow the W3C SSE text
protocol. Every frame is one `event:` line plus one `data:` line
(JSON object) plus a terminating blank line. There are no bare
heartbeats and no multi-line data frames.

Event types:

| `event:`       | Payload                                                                      |
|----------------|------------------------------------------------------------------------------|
| `thinking`     | `{"text": "..."}` — model surface-reasoning, if the model emits it.          |
| `tool_call`    | `{"name": "opencost.allocation", "args": {...}}` — MCP invocation announced. |
| `tool_result`  | `{"name": "...", "content": "..."}` — MCP server response passed through.    |
| `token`        | `{"text": "..."}` — one chunk of assistant text.                             |
| `done`         | Final frame: `request_id`, `model`, `done_reason`, `usage`, `tool_calls`, `total_duration_ms`, `latency_ms`. |
| `error`        | Replaces `done` when the stream fails mid-flight: `request_id`, `status`, `title`, `detail`. |

Example (`\n\n` delimits frames):

```
event: tool_call
data: {"name":"opencost.allocation","args":{"window":"yesterday"}}

event: token
data: {"text":"Yesterday the "}

event: token
data: {"text":"`platform` namespace spent **$42.17** "}

event: done
data: {"request_id":"0a1b2c3d4e5f6789","model":"qwen2.5:7b-instruct","usage":{"prompt_tokens":412,"completion_tokens":187},"latency_ms":1843}
```

Headers set on the streaming response:

- `Content-Type: text/event-stream`
- `Cache-Control: no-cache`
- `Connection: keep-alive`
- `X-Accel-Buffering: no` (defeats nginx buffering)

Reverse proxies in front of the gateway MUST NOT buffer SSE.

Error handling during streaming: once the first byte of the SSE
body has been flushed, a failure cannot change the HTTP status.
The handler emits an `event: error` frame and closes. Clients MUST
be prepared to see an `error` event at any point before `done`.

### 4.3 `GET /v1/tools`

Lists MCP tools the bridge has discovered. **v0.1 limitation:**
`jonigl/ollama-mcp-bridge` does not expose a tool-listing endpoint,
so this route returns an empty list with `discovery_deferred:true`.
Clients SHOULD render "tool discovery not yet supported" rather
than assuming the bridge is misconfigured.

- Auth: required.
- Rate-limited: no (cheap, idempotent).
- Response `200` (`apiv1.ToolsResponse`):

```json
{"tools": [], "discovery_deferred": true}
```

When the bridge (or the gateway cache) grows discovery support the
array populates and `discovery_deferred` goes false.

### 4.4 `GET /v1/models`

Lists Ollama models reachable through the bridge. Proxies
`/api/tags` and reshapes the entries into `apiv1.Model`.

- Auth: required.
- Rate-limited: no.
- Response `200` (`apiv1.ModelsResponse`):

```json
{
  "models": [
    {
      "name": "qwen2.5:7b-instruct",
      "digest": "sha256:...",
      "size": 4683073395,
      "modified_at": "2026-04-15T10:22:03Z",
      "family": "qwen2",
      "parameter_size": "7B",
      "quantization": "Q4_K_M"
    }
  ],
  "default": "qwen2.5:7b-instruct"
}
```

An empty `models` array is not an error — a freshly-installed
Ollama with no pulled models is a legitimate state that operators
render as "no models installed".

### 4.5 `GET /metrics` (separate listener)

Prometheus text exposition, v0.0.4. Served on the **separate**
listener bound to `OPENCOST_AI_METRICS_LISTEN_ADDR` (default
`127.0.0.1:9090`) — not on the main API listener.

Exposed series (pre-registered, so `absent_over_time` checks are
sensible even before the first request):

| Metric                                              | Type      | Labels                     |
|-----------------------------------------------------|-----------|----------------------------|
| `opencost_ai_gateway_requests_total`                | counter   | `endpoint, method, status` |
| `opencost_ai_gateway_request_duration_seconds`      | histogram | `endpoint, method`         |
| `opencost_ai_gateway_requests_in_flight`            | gauge     | —                          |
| `opencost_ai_gateway_tool_calls_total`              | counter   | `tool`                     |
| `opencost_ai_gateway_tool_call_duration_seconds`    | histogram | `tool`                     |
| `opencost_ai_gateway_model_tokens_total`            | counter   | `model, kind`              |
| `opencost_ai_gateway_upstream_errors_total`         | counter   | `op, kind`                 |
| `opencost_ai_gateway_rate_limited_total`            | counter   | —                          |

`kind` on `model_tokens_total` is `"prompt"` or `"completion"`.
`kind` on `upstream_errors_total` is `"transport"`, `"http_4xx"`,
`"http_5xx"`, `"http_other"`, or `"unknown"`.

Default go-runtime and process collectors are deliberately
**not** registered — the gateway's metric surface is a stable
contract and must not silently follow upstream default changes.

The `/metrics` endpoint is not authenticated. Scope access at the
network layer (the shipped NetworkPolicy scopes same-namespace
only by default; override
`networkPolicy.metricsIngress.allowedFrom` for a
cross-namespace Prometheus). The ServiceMonitor template in the
chart wires Prometheus Operator scraping when enabled.

## 5. Rate limiting

- Scope: per caller identity (SHA-256 prefix of the bearer token).
- Algorithm: token bucket, `OPENCOST_AI_RATE_LIMIT_PER_MIN`
  tokens/min with a burst of the same size (default 60/min).
- `0` or a negative value disables limiting entirely.
- Only `POST /v1/ask` is limited. The read-only routes (`/tools`,
  `/models`, `/health`) are intentionally unlimited.
- Exceeding the bucket returns `429 Too Many Requests` with a
  problem+json detail that states the configured per-minute rate.
- Every rejection increments
  `opencost_ai_gateway_rate_limited_total`.

## 6. Errors (RFC 7807 problem+json)

Every non-2xx response has this shape
(`apiv1.Problem`, Content-Type `application/problem+json`):

```json
{
  "type": "about:blank",
  "title": "Bad Request",
  "status": 400,
  "detail": "request body has unknown field \"foo\"",
  "instance": "/v1/ask#0a1b2c3d4e5f6789",
  "request_id": "0a1b2c3d4e5f6789"
}
```

| Status | Title                     | When                                                                                  |
|--------|---------------------------|---------------------------------------------------------------------------------------|
| 400    | Bad Request               | Invalid JSON, unknown field, trailing tokens, bad UUID, empty query, oversized query. |
| 401    | Unauthorized              | Missing header, wrong scheme, empty/invalid bearer token.                             |
| 413    | Payload Too Large         | Body exceeds `OPENCOST_AI_MAX_REQUEST_BYTES`.                                         |
| 415    | Unsupported Media Type    | `Content-Type` is not `application/json`.                                             |
| 429    | Too Many Requests         | Per-caller rate limit exceeded.                                                       |
| 500    | Internal Server Error     | Unexpected programming error or auth validator fault.                                 |
| 502    | Bad Gateway               | Bridge transport failure, bridge 4xx/5xx (except 503), or unknown upstream error.     |
| 503    | Service Unavailable       | No auth token configured on disk, or bridge returned 503.                             |

`detail` strings are caller-safe and describe the violation, never
the caller payload. Upstream status codes are intentionally
collapsed into 502/503 so the gateway does not leak bridge
framing.

`instance` threads the request path and ID together. `request_id`
is an RFC 7807 extension member so clients do not have to parse
`instance` to correlate with audit logs.

## 7. Headers

| Header                  | Direction | Notes                                                                                          |
|-------------------------|-----------|------------------------------------------------------------------------------------------------|
| `Authorization`         | request   | `Bearer <token>` on every authenticated endpoint.                                              |
| `Content-Type`          | request   | `application/json` required when a body is sent.                                               |
| `X-Request-ID`          | both      | Caller may supply; gateway validates and echoes, else generates. Echoed on every response.     |
| `WWW-Authenticate`      | response  | Set to `Bearer realm="opencost-ai"` on every 401.                                              |
| `Cache-Control`         | response  | `no-cache` on SSE responses.                                                                   |
| `Connection`            | response  | `keep-alive` on SSE responses.                                                                 |
| `X-Accel-Buffering`     | response  | `no` on SSE responses.                                                                         |

## 8. Worked examples

### 8.1 Single-shot query (non-streaming)

```sh
TOKEN=$(cat /var/run/secrets/opencost-ai/token)
curl -fsS \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"query":"what did yesterday cost?"}' \
  http://opencost-ai-gateway.opencost-ai.svc:8080/v1/ask
```

### 8.2 Streaming query

```sh
curl -N -fsS \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{"query":"break down last week by namespace","stream":true}' \
  http://opencost-ai-gateway.opencost-ai.svc:8080/v1/ask
```

`-N` disables curl's output buffering so events surface as they
arrive.

### 8.3 Liveness probe

```sh
curl -fsS http://opencost-ai-gateway.opencost-ai.svc:8080/v1/health
# {"status":"ok","version":"0.1.0"}
```

No auth header; no request body.

### 8.4 List installed models

```sh
curl -fsS \
  -H "Authorization: Bearer ${TOKEN}" \
  http://opencost-ai-gateway.opencost-ai.svc:8080/v1/models
```

## 9. Versioning and compatibility

- The contract above is `/v1`. Additive fields on responses are
  allowed without a bump; clients must tolerate unknown JSON
  fields.
- Renames, removals, and semantic changes to existing fields wait
  for `/v2`.
- The SSE frame *types* (`thinking`, `tool_call`, `tool_result`,
  `token`, `done`, `error`) are part of the contract. New frame
  types may land additively; unknown types must be ignored by
  clients.
- Audit log JSON shape is documented in
  `internal/audit/audit.go` and is a separate contract (see
  `docs/security.md` for what is and is not logged).
