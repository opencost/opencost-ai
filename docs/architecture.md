# opencost-ai — Current-State Analysis and Forward Design

**Author prep for:** Warwick Peatey (Kubecost / OpenCost maintainer)
**Target consumer:** Claude Code, for further development
**Date:** 2026-04-16
**Upstream repo reviewed:** https://github.com/opencost/opencost-ai @ `main` (3 commits, 0 stars, 0 issues, 1 drive-by README PR)

---

## 1. Executive summary

`opencost-ai` is not a product today. It is a ~160-line Dockerfile plus a ~210-line Flask script that shells out to a TUI (`ollmcp`) and scrapes its output with regex. The container bakes Ollama, `ollmcp`, Flask, and a tiny 0.5B model into a single monolithic image to talk to the OpenCost MCP server (already built-in to OpenCost as of v1.118).

The *intent* — a local, air-gap-friendly, open-source AI assistant for OpenCost — is sound and fills a real gap. The *execution* as committed is a prototype that should not be built on. Specifically:

- **The core integration mechanism is wrong.** Driving an interactive TUI with `pexpect` and parsing ANSI-stripped regex groups out of the output is brittle by design. Any version bump to `ollmcp` breaks it silently.
- **Security posture is absent.** Runs as root, no authentication, no input validation, CORS wide open, binds `0.0.0.0`, subprocess spawn for every request with a 5-minute timeout each.
- **No tests, no CI, no Helm chart, no README, no versioning.** The repo is a personal experiment pushed to the org namespace.
- **Ollama-in-container is the wrong unit of deployment** for the stated air-gap use case. It couples model lifecycle to container rebuilds and makes GPU passthrough, model caching, and multi-tenant use harder, not easier.

The good news: there's a clean path forward that preserves the stated goal (local, open, air-gap-capable), reuses the OpenCost MCP server as-is, and swaps the brittle TUI-scraping for a first-class programmatic bridge. The bulk of this document is that design.

---

## 2. What's actually in the repo

Full inventory:

```
opencost-ai/
├── LICENSE                        # Apache-2.0
├── docker/
│   └── Dockerfile.ollama          # ~160 lines; Ollama + ollmcp + Flask in one image
└── src/
    └── ollmcp-api-server.py       # ~210 lines; Flask wrapper around ollmcp TUI
```

No README, no CONTRIBUTING, no tests, no CI, no Helm chart, no compose file, no `requirements.txt` (pinned in the Dockerfile only), no changelog, no `.gitignore`.

### 2.1 `ollmcp-api-server.py` — the Flask wrapper

Three endpoints:

- `GET /health` — returns `{"status":"healthy","default_model":"qwen2.5:0.5b"}`.
- `GET /models` — shells out to `ollama list`, parses stdout line-by-line.
- `GET /tools` — spawns `ollmcp`, sends `quit`, greps the welcome screen for `✓ opencost.*`.
- `POST /query` — the main path. Spawns `ollmcp` via `pexpect`, waits for the `❯` prompt, sends `hil` to disable human-in-the-loop, sends the query, waits on a 5-minute timeout, optionally answers `y` to a confirmation prompt, sends `quit`, then regex-parses the captured output for `📝 Answer (Markdown):` blocks, strips ANSI escapes and box-drawing characters, and returns whatever survives.

Concrete problems, in order of severity:

1. **TUI scraping as the integration contract.** `ollmcp` is an interactive terminal UI whose output format is an implementation detail, not an API. The parser already needs a primary regex, a fallback regex, and a third last-resort line-filter loop with a skip-list of emoji. Any version of `ollmcp` that changes a prompt character, a banner, or an emoji breaks production. This cannot be the interface.
2. **No authentication.** `/query` executes LLM calls against infrastructure data. Anyone on the pod network can call it. No API key, no mTLS, no service-account check.
3. **Subprocess per request.** Every `/query` call spawns a new `ollmcp` process, which reconnects to the MCP server, lists tools, loads the model context, and tears down. Startup cost is multiples of query cost for small queries.
4. **No request concurrency control.** Flask dev server (`app.run`), no WSGI, no rate limit, no queue. A second request while the first is running will fight for the same `ollmcp` session depending on how the kernel schedules the spawn.
5. **Input goes straight into a shell-adjacent context.** `user_query` is typed into a `pexpect` session via `sendline`. It's not obvious what happens when the query contains `\n`, backtick, or a line starting with a slash-command that `ollmcp` would intercept. Needs adversarial testing before exposure.
6. **Errors leak internals.** `except Exception as e: return jsonify({"error": str(e)})` returns raw exception strings — pexpect tracebacks, stack state, config paths.
7. **Hardcoded assumptions.** `/root/.config/ollmcp/servers.json`, port 8888, Unicode `❯` as expect token, `qwen2.5:0.5b` as default.
8. **No structured logging, no metrics, no tracing.** Not even a per-request ID.

### 2.2 `Dockerfile.ollama` — the container

Builds from `ollama/ollama:latest`, installs Python + `ollmcp` + `flask` + `pexpect` into a venv, pre-pulls `qwen2.5:0.5b` at build time (configurable via `--build-arg`), writes a startup script that rewrites `servers.json` from `$MCP_SERVER_URL`, starts `ollama serve`, waits 30s for it, probes the OpenCost MCP server with a JSON-RPC `initialize` call, starts the Flask API, and does `wait -n` on both PIDs.

Concrete problems:

1. **`FROM ollama/ollama:latest`** — non-reproducible base, no digest pin, no SBOM.
2. **Runs as root.** No `USER` directive.
3. **Model baked into image.** A 0.5B model is ~400MB. A useful model (7B Q4) is 4–5GB. Baking into the image couples model updates to image rebuilds, breaks caching at the registry and OCI layer level, and makes air-gap deployments ship the model through the image registry rather than a dedicated artifact store. Ollama has a volume-mounted model cache for exactly this reason.
4. **`qwen2.5:0.5b` is too small to be useful for tool use.** Tool-calling quality falls off a cliff below ~3B. This default will produce bad answers out of the box, which is the worst possible first impression.
5. **`apt-get update` with no cache cleanup audit, `latest` tags, no `--no-install-recommends`** on the apt layer.
6. **Shell heredoc with `COPY <<EOF`** for the startup script — harder to test, harder to lint, harder to rebuild incrementally than a file.
7. **Health probe is a curl against the MCP server inside the startup script** that continues "anyway" on failure. The container reports healthy when the MCP path is dead.
8. **No `HEALTHCHECK` directive, no resource hints, no labels.**

### 2.3 What's right about the repo

To be fair:

- **The overall topology is correct:** client → local LLM runtime with tool use → OpenCost MCP server → OpenCost HTTP API. That's the right shape.
- **Apache-2.0 is the right license** for an OpenCost-org project.
- **Choosing MCP as the contract to OpenCost** is correct — OpenCost ships a built-in MCP server on port 8081 as of v1.118, and building against that is strictly better than reinventing an LLM-specific API.
- **Air-gap-first framing is the right differentiator.** Kubecost users in regulated industries, defense, and finance cannot use hosted AI, and the current landscape is dominated by hosted offerings.

---

## 3. Stated goal vs. current state

Your stated goal: *"a LOCAL AI running that would support air-gapped installations and be open source."*

The gap between that goal and the current code, stated plainly:

| Requirement (stated) | Current state | Gap |
|---|---|---|
| Local inference | ✅ Ollama in-container | Model choice is too small; no GPU story |
| Air-gap capable | ⚠️ Partial | Image pulls from `ollama/ollama:latest`; model pulled at build; no offline install flow documented |
| Open source | ✅ Apache-2.0 | Fine |
| OpenCost integration | ⚠️ via `ollmcp` TUI scrape | Wrong integration boundary — see §2.1 |
| Production deployable | ❌ | No Helm chart, no auth, runs as root, no tests, Flask dev server |
| Maintainable | ❌ | Zero docs, zero tests, brittle regex parser |

---

## 4. Landscape check — what to build on

Before writing new code, three upstream projects matter:

### 4.1 OpenCost MCP server (built-in, v1.118+)

Already exists. Runs on port 8081 in every default Helm install. Exposes three tools: cost allocation, asset cost, cloud cost — with filtering and aggregation. This is our data plane. We do not touch it; we consume it.

### 4.2 `ollama-mcp-bridge` (jonigl)

Same author as `ollmcp`. Critical architectural alternative: it's a **transparent proxy in front of the Ollama API** (FastAPI-based) that pre-loads MCP servers at startup, injects their tools into every `/api/chat` request, handles multi-round tool execution, and streams responses. It is a drop-in Ollama replacement — existing Ollama clients point at the bridge URL instead of Ollama and get MCP tools for free.

This matters because it replaces the entire "Flask wrapper scraping a TUI" layer with a battle-tested proxy that speaks the native Ollama `/api/chat` contract. No regex. No `pexpect`. No TUI. The right answer for the `/query` path in this project is almost certainly "use `ollama-mcp-bridge`, don't reinvent it."

### 4.3 `ollmcp` (jonigl)

`ollmcp` is a **TUI for humans**. It is not an API. It was never meant to be scripted. The current repo is using it as an API because that's what was reachable in a weekend prototype. Do not continue down this path.

---

## 5. Product direction — `opencost-ai` v0.1

Scope the v0.1 deliberately narrow so it can ship and get real usage feedback. Everything below is a recommendation; push back on any of it with counter-evidence.

### 5.1 Product thesis

> A Kubernetes-native, air-gap-deployable, open-source AI assistant for OpenCost that lets platform teams ask cost questions in natural language without sending cluster data to a third-party LLM provider.

What that phrase excludes, intentionally:

- **Not a general chatbot.** It answers OpenCost-derived questions.
- **Not a cost-recommendation engine** (yet). Generating "you should rightsize X" requires evaluation harnesses we don't have. v0.1 exposes existing data through language; it does not prescribe.
- **Not a multi-cluster federated system.** One OpenCost instance per deployment.
- **Not hosted.** No SaaS in the open-source project.

### 5.2 Target users

1. **Platform / FinOps engineers** running OpenCost in an air-gapped or sovereign cluster (DoD, regulated finance, EU data-residency).
2. **Kubecost users** evaluating whether the OSS AI path is credible before asking for a managed equivalent.
3. **OpenCost contributors** who want an AI dev-experience against their local cluster without signing up for anything.

### 5.3 Non-goals for v0.1

- Cost forecasting or anomaly detection models.
- Fine-tuned / domain-specific models. Shipping a generic tool-use-capable model is good enough.
- Web UI. CLI + OpenAPI-compatible HTTP endpoint only; UI comes after the API is stable.
- Authenticated multi-tenancy. SPIFFE-style cluster identity is enough; user-level auth is v0.2.

---

## 6. Target architecture

```
┌───────────────────────────────────────────────────────────────────┐
│ Kubernetes cluster (air-gapped)                                   │
│                                                                   │
│   ┌────────────────┐   kubectl/curl   ┌──────────────────────┐    │
│   │ Platform user  │ ───────────────▶ │ opencost-ai-gateway  │    │
│   └────────────────┘                  │ (Go, thin HTTP API)  │    │
│                                       └────────┬─────────────┘    │
│                                                │ /api/chat         │
│                                                ▼                  │
│                                 ┌──────────────────────────────┐  │
│                                 │ ollama-mcp-bridge            │  │
│                                 │ (FastAPI, upstream OSS)      │  │
│                                 └──┬───────────────────┬───────┘  │
│                                    │                   │          │
│                         MCP tools  │                   │ inference│
│                                    ▼                   ▼          │
│                        ┌──────────────────┐   ┌────────────────┐  │
│                        │ OpenCost exporter│   │ Ollama         │  │
│                        │ + MCP svr :8081  │   │ (GPU optional) │  │
│                        └──────────────────┘   └────────────────┘  │
└───────────────────────────────────────────────────────────────────┘
```

Four containers, three of them upstream and untouched:

1. **`opencost-ai-gateway`** — the only thing we own and ship. Go, thin, auth + audit + quota + prompt-shaping. Documented below.
2. **`ollama-mcp-bridge`** — upstream, packaged in our Helm chart, configured to point at OpenCost's MCP endpoint.
3. **`ollama`** — upstream, with a PVC for the model cache so models survive pod restarts and aren't baked into images.
4. **OpenCost** — upstream, already shipping the MCP server.

### 6.1 Why a Go gateway in front of `ollama-mcp-bridge`?

Because the bridge speaks the Ollama `/api/chat` contract, which is *intentionally unauthenticated* (it's designed for trusted localhost). For a cluster-exposed API we need:

- Authentication (start with static bearer token, then SPIFFE/SPIRE).
- Per-caller rate limits.
- Audit logging of the query (*not* the completion — cost data is sensitive).
- Prompt guardrails — a system prompt that scopes the model to OpenCost questions.
- Result post-processing and optional schema enforcement (e.g. return structured JSON for UI consumption).
- A small, stable HTTP surface (`POST /v1/ask`, `GET /v1/health`, `GET /v1/tools`, `GET /v1/models`) decoupled from whatever Ollama's evolving `/api/chat` shape is.

Go, not Python, because:
- Aligns with Kubecost/OpenCost codebase skills.
- Smaller, statically linked container; simpler SBOM; faster cold start.
- Easier to share types with OpenCost if we ever inline the MCP client.

### 6.2 Why keep Ollama as the inference runtime?

- Ollama has model-format standardization (GGUF), a cache, an Ollama Registry, and offline `ollama create` from GGUF files — all of which matter for air-gap.
- Swappable later. The gateway only sees `ollama-mcp-bridge`; swapping to vLLM or llama.cpp server is a bridge-level concern.

### 6.3 Model recommendation for v0.1 defaults

Tool use is the hard requirement; reasoning quality is secondary. Candidates and trade-offs:

| Model | Size (Q4) | Tool use | Notes |
|---|---|---|---|
| `qwen2.5:0.5b` *(prototype default)* | 0.4 GB | Poor | CI smoke-test only; do not ship |
| `qwen2.5:7b-instruct` *(v0.1 default)* | 4.7 GB | Good | Apache 2.0; ~6 GB VRAM floor |
| `llama3.1:8b-instruct` | 4.9 GB | Good | Meta Llama 3 Community License; documented override |
| `mistral-nemo:12b` | 7.1 GB | Good | Apache 2.0; best reasoning, ~10 GB VRAM floor; documented upgrade path |

Ship `qwen2.5:7b-instruct` as the default, exposed via Helm values key
`ollama.defaultModel` so operators can substitute `mistral-nemo:12b` or
`llama3.1:8b-instruct` without rebuilding. README states the VRAM/RAM
floor for each option and lists the override command.

---

## 7. `opencost-ai-gateway` v0.1 specification

### 7.1 HTTP surface

```
POST /v1/ask              # main endpoint
GET  /v1/tools            # list MCP tools discovered through the bridge
GET  /v1/models           # list installed Ollama models
GET  /v1/health           # liveness + dependency readiness
GET  /v1/version          # build metadata (git SHA, version, SBOM hash)
GET  /metrics             # Prometheus metrics
```

### 7.2 `POST /v1/ask` contract

Request:
```json
{
  "query": "string, required, max 4KB",
  "model": "string, optional; defaults to server config",
  "stream": false,
  "format": "text|json",
  "conversation_id": "optional uuid for multi-turn"
}
```

Response (non-streaming):
```json
{
  "request_id": "uuid",
  "model": "qwen2.5:7b-instruct",
  "query": "echoed",
  "answer": "markdown",
  "tool_calls": [
    {"name": "opencost.allocation", "args": {...}, "duration_ms": 142}
  ],
  "usage": {"prompt_tokens": 412, "completion_tokens": 187},
  "latency_ms": 1843
}
```

Response (streaming): SSE, events typed as `thinking`, `tool_call`, `tool_result`, `token`, `done`. Same schema as native Ollama streaming, wrapped.

Errors: problem+json (RFC 7807) — no raw exception strings ever.

### 7.3 Authentication

- v0.1: static bearer token read from a Kubernetes Secret. `Authorization: Bearer <token>`. Rotate via Secret update; gateway watches for changes.
- v0.2: SPIFFE/SPIRE workload identity. Documented as a follow-up.

### 7.4 System prompt (ships with gateway, configurable via ConfigMap)

Constrains model behavior to:
- Use MCP tools for cost data; never invent numbers.
- If a tool call fails, say so explicitly; do not hallucinate a fallback answer.
- Return markdown formatted for terminal and web readability.
- Refuse to answer questions unrelated to Kubernetes cost / OpenCost data.

### 7.5 Security requirements (non-negotiable)

- Runs as non-root UID 65532 with a read-only root filesystem.
- No host network, no privileged, no `hostPath` mounts.
- NetworkPolicy shipped in the Helm chart: egress only to the configured bridge + Ollama + (if needed) OpenCost MCP; no internet.
- PodSecurity `restricted` compliant.
- Images signed with cosign; SBOM published per release.
- Distroless or Chainguard base.
- All inputs length-validated, content-type-checked, and rejected on unexpected fields.
- Structured audit log to stdout with request ID, caller identity, timestamp, model, token counts, tool calls, **but not the query text or completion text** unless explicitly enabled per-deployment (opt-in, off by default).

### 7.6 Observability

- Prometheus metrics: request count by endpoint/status, latency histograms, in-flight requests, tool-call count and duration, per-model token totals, upstream error rate.
- OTLP tracing optional, off by default, configurable endpoint.
- Log format: JSON, one line per event, `slog`-style.

### 7.7 Configuration (env + ConfigMap)

```
OPENCOST_AI_BRIDGE_URL         default: http://ollama-mcp-bridge:8000
OPENCOST_AI_LISTEN_ADDR        default: :8080
OPENCOST_AI_DEFAULT_MODEL      default: qwen2.5:7b-instruct
OPENCOST_AI_REQUEST_TIMEOUT    default: 120s
OPENCOST_AI_MAX_REQUEST_BYTES  default: 8192
OPENCOST_AI_AUDIT_LOG_QUERY    default: false
OPENCOST_AI_AUTH_TOKEN_FILE    default: /var/run/secrets/opencost-ai/token
```

### 7.8 Code layout (for Claude Code's initial scaffold)

Language: current stable Go (1.26 as of initial commit). `go.mod` and
the CI/build toolchain track the same line; this is a greenfield repo
with no consumers, so there is no reason to pin below current stable.

```
opencost-ai/
├── CLAUDE.md                   # project-level instructions for Claude Code
├── README.md
├── LICENSE                     # existing Apache-2.0
├── SECURITY.md
├── CONTRIBUTING.md
├── CODEOWNERS
├── .github/
│   └── workflows/
│       ├── ci.yml              # build + test + lint + SLSA provenance
│       ├── release.yml         # cosign-signed images + SBOM
│       └── codeql.yml
├── cmd/
│   └── gateway/main.go
├── internal/
│   ├── server/                 # HTTP handlers, middleware
│   ├── auth/                   # bearer-token validator, token file watcher
│   ├── bridge/                 # ollama-mcp-bridge client
│   ├── prompt/                 # system prompt loader, validator
│   ├── audit/                  # structured audit log
│   ├── ratelimit/              # token-bucket per-caller limiter
│   └── config/                 # env + file loader, validation
├── pkg/
│   └── apiv1/                  # exported request/response types for SDKs
├── deploy/
│   ├── helm/opencost-ai/       # Helm chart: gateway + bridge + ollama
│   └── examples/
│       ├── air-gapped.md
│       └── dev-local/
├── test/
│   ├── integration/            # against kind + helm install
│   └── e2e/                    # against real OpenCost
└── docs/
    ├── architecture.md
    ├── security.md
    ├── air-gap-install.md
    └── prompts.md
```

`CLAUDE.md` at the root is important per your standing preference. It should encode: never commit secrets, use signed commits (your existing `opencost-contributor` skill already covers this), prefer stdlib over dependencies in `internal/`, keep the gateway under 2000 LOC.

---

## 8. Migration from current code

The current `src/ollmcp-api-server.py` and `docker/Dockerfile.ollama` should be **archived, not extended**. Specifically:

- Move both files into `legacy/prototype-flask/` with a README noting the prototype's purpose and why it was replaced.
- New development starts clean in `cmd/gateway/` and `deploy/helm/`.
- The one-page `/query` contract from the prototype can inform the `/v1/ask` contract, but nothing else in that file is worth carrying over.

This is a judgment call — you could incrementally refactor, but the rewrite surface is larger than the rewrite-from-scratch surface.

---

## 9. Delivery plan — v0.1 in ~6 weeks

Sized for Warwick's TAU methodology (1 BE + 2 FE-capable contributors), but FE work is minimal in v0.1 so it's really 2 backend people + a reviewer.

| Week | Work |
|---|---|
| 1 | Scaffold Go gateway; CI/CD; distroless image; cosign signing; integration test harness (kind + OpenCost + bridge + Ollama with `qwen2.5:0.5b` smoke model). |
| 2 | `POST /v1/ask` happy path against the bridge. System prompt + guardrails. Problem+json errors. Bearer-token auth + token-file watcher. |
| 3 | Streaming SSE. Rate limit. Audit log. Prometheus metrics. |
| 4 | Helm chart: gateway + bridge + Ollama with PVC. NetworkPolicy. PodSecurity. ServiceMonitor. |
| 5 | Air-gap install flow documented end-to-end: `ollama pull` on a connected machine → `ollama save` to GGUF → OCI artifact → internal registry → `ollama create` in-cluster. Validated on a disconnected kind cluster. |
| 6 | Docs pass; threat model writeup; release `v0.1.0` with signed images and SBOM; community announcement. |

Explicit out-of-scope for v0.1: streaming multi-turn conversations with persisted history, per-user auth, fine-tuned models, evaluation harness, web UI.

---

## 10. Decisions

Resolved by project lead (Warwick Peatey, 2026-04-16). Claude Code
treats these as settled and implements against them.

1. **MCP transport: Streamable HTTP** (MCP spec 2025-03-26). Gateway and
   bridge standardize on this. A one-hour spike in Session 1 confirms
   the OpenCost MCP server (v1.118+) serves it correctly; if it does
   not, stop and escalate rather than fall back.
2. **Bridge `servers.json` transport string: `streamable_http`.** OpenCost
   advertises `type: "http"` at its endpoint — that's the endpoint
   description, not the bridge client config. Bridge config names it
   explicitly.
3. **Model weights in air-gap: OCI registry via ORAS.** Reuses existing
   container-registry auth, mirroring, and signing. Documented
   end-to-end in `docs/air-gap-install.md` per Session 5.
4. **Helm chart home: `opencost-ai` repo** (this repo). Separate release
   cadence from OpenCost core. Migration to `opencost-helm-chart` is
   deferred to v1.0 and out of scope.
5. **Default model: `qwen2.5:7b-instruct` with Helm override.** Values
   key `ollama.defaultModel` lets operators substitute
   `mistral-nemo:12b` (better reasoning, ~10 GB VRAM floor) or
   `llama3.1:8b-instruct` without rebuilding. README states the VRAM
   floor (~6 GB for the 7B default, ~10 GB for the 12B upgrade) and
   lists the override command. `mistral-nemo:12b` is the documented
   upgrade path for operators with headroom. No bundled-weights
   licensing check is needed because all three candidates are Apache 2.0.

---

## 11. Hand-off summary for Claude Code

- **Start here:** create `CLAUDE.md` at the repo root per §7.8, then scaffold the Go gateway per the same section.
- **Do not extend** `src/ollmcp-api-server.py`. Move it to `legacy/` with a note.
- **Do not reinvent** what `ollama-mcp-bridge` already does. Consume it.
- **Security requirements in §7.5 are non-negotiable** — do not ship code that runs as root, binds without auth, or leaks raw exceptions.
- **Open questions in §10 should be answered before coding**, not assumed away. (Resolved 2026-04-16 — see §10.)
