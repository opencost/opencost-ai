# Security model — opencost-ai

Threat model and operator-facing security guidance for
`opencost-ai` at v0.1.0. Complements `SECURITY.md` (reporting
policy) and `docs/architecture.md` §7.5 (non-negotiable
requirements) with a STRIDE-driven walk of the system.

## 1. Scope and posture

**In scope:** the `opencost-ai-gateway` binary and container image,
the Helm chart at `deploy/helm/opencost-ai/`, and the air-gap
install flow under `scripts/air-gap/` + `docs/air-gap-install.md`.

**Out of scope:** upstream `jonigl/ollama-mcp-bridge`, upstream
Ollama, the OpenCost exporter and its MCP server. Findings in
those projects are coordinated with their maintainers; see
`SECURITY.md`.

**Design posture (non-negotiables — see `CLAUDE.md`):**

- Gateway runs as non-root UID 65532, read-only root filesystem,
  no `hostPath` / `hostNetwork` / `privileged`, all Linux
  capabilities dropped.
- All `/v1/*` endpoints except `/v1/health` require a bearer
  token. No anonymous access.
- Errors are RFC 7807 problem+json with caller-safe strings;
  upstream framing never reaches the caller.
- Query text and completion text are **not** logged by default.
  The `OPENCOST_AI_AUDIT_LOG_QUERY` opt-in is off by default and
  stays that way.
- No component is granted internet egress. The shipped
  NetworkPolicy allows only the minimum topology: gateway →
  bridge, bridge → Ollama + OpenCost MCP, Ollama → kube-dns.

## 2. Assets

What an attacker plausibly wants from this system, in decreasing
order of sensitivity:

| Asset                              | Why it matters                                                                                     |
|------------------------------------|----------------------------------------------------------------------------------------------------|
| **Cluster cost data**              | Reveals workload footprints, team/project spend, third-party service usage, compliance posture.    |
| **Bearer token**                   | Grants `POST /v1/ask` access; an attacker with the token can run arbitrary queries against costs.  |
| **MCP tool catalogue / responses** | Leaks the shape of OpenCost's inventory (namespaces, node types, tenants) even without raw numbers. |
| **Model weights on the PVC**       | Usually public weights today, but air-gap operators may have site-licensed weights on the same PVC. |
| **Audit log stream**               | Contains caller identity (token digest prefix), token counts, tool names — enough for traffic analysis. |
| **Gateway metrics**                | Per-model token counters + per-tool invocation counters leak usage patterns even absent raw data.  |

## 3. Trust boundaries

```
┌──────────────────────────────── CLUSTER ─────────────────────────────┐
│                                                                      │
│  ⟨ caller pod ⟩  ──(1)──►  ⟨ gateway ⟩  ──(2)──►  ⟨ bridge ⟩         │
│                                │                      │              │
│                                │ (3) metrics          │ (4)          │
│                                ▼                      ▼              │
│                           ⟨ Prometheus ⟩        ⟨ ollama ⟩   ⟨ opencost-mcp ⟩
│                                                                      │
│  ⟨ Secret: auth token ⟩  ─(mount)─►  /var/run/secrets/opencost-ai/token
│                                                                      │
└──────────────────────────────────────────────────────────────────────┘
```

Each numbered edge crosses a trust boundary and is subject to
its own controls:

1. **Caller → gateway:** authenticated (bearer token), rate-limited
   per caller, request-size-bounded, content-type-checked.
2. **Gateway → bridge:** unauthenticated but network-segmented via
   NetworkPolicy. Same-namespace only by default.
3. **Gateway → Prometheus:** unauthenticated, network-segmented,
   separate loopback-by-default listener.
4. **Bridge → Ollama / OpenCost MCP:** unauthenticated, network-
   segmented, egress-restricted to two specific services.

## 4. STRIDE analysis

STRIDE is applied per-component, not per-asset. For each
component the relevant categories are called out; categories
that do not apply in v0.1 are noted explicitly so absence is
not ambiguous.

### 4.1 Gateway HTTP listener (`POST /v1/ask` and friends)

**Spoofing**

- *Threat:* a caller in the cluster network impersonates a valid
  client by replaying or forging a bearer token.
- *Mitigations:*
  - Static bearer token loaded from a Kubernetes Secret; operators
    provision high-entropy tokens (`openssl rand -hex 32`).
  - Constant-time comparison (`subtle.ConstantTimeCompare`) on
    equal-length inputs avoids timing-based token recovery.
  - 401 responses carry `WWW-Authenticate: Bearer realm=...`;
    error detail strings never echo the submitted token.
  - `internal/auth/source.go` watches the token file for mtime
    advances and picks up rotations without a restart.
- *Residual risk:* token theft (Secret leak, sidecar compromise,
  log spill in another tool mounting the token) remains the
  dominant compromise vector. v0.2 SPIFFE/SPIRE identity is the
  documented mitigation path.

**Tampering**

- *Threat:* request-body manipulation to smuggle oversized
  payloads, unknown fields, or non-JSON content into the handler.
- *Mitigations:*
  - `http.MaxBytesReader` enforces `OPENCOST_AI_MAX_REQUEST_BYTES`
    (default 8192) *before* `json.Decoder` touches the body, so
    a 50 MB blob cannot exhaust memory.
  - `DisallowUnknownFields` + an "exactly one JSON value" check
    (`internal/server/handlers.go`) reject unknown fields and
    trailing tokens.
  - Per-field validation: `Query` has its own 4 KiB ceiling,
    must be valid UTF-8, must be non-empty after trim;
    `ConversationID` must be UUID-shaped.
  - `Content-Type` must be `application/json`; anything else is
    415.
- *Residual risk:* none known inside the decoding path in v0.1.

**Repudiation**

- *Threat:* a caller denies having issued a query that matches an
  unauthorised access pattern.
- *Mitigations:*
  - Every authenticated request is audited as a single JSON line
    with request ID, SHA-256-prefix caller identity, model, token
    counts, tool names, status, outcome, and latency
    (`internal/audit/audit.go`).
  - `X-Request-ID` threads through response headers,
    problem+json `instance`, and every audit event, so a client
    record (the header they saw) and a server record (the audit
    line) are bilaterally linkable.
  - Audit writes serialise through a mutex so two concurrent
    requests cannot interleave within a single JSON line.
- *Residual risk:* caller identity is a pseudonym (token hash
  prefix), not a user principal. Repudiation is tractable only at
  token granularity in v0.1; per-user non-repudiation waits for
  SPIFFE/SPIRE in v0.2.

**Information disclosure**

- *Threat:* error responses, logs, or metrics leak caller data,
  upstream framing, or token material.
- *Mitigations:*
  - Problem+json `detail` strings are hand-authored and
    caller-safe; `decodeErrorDetail` in
    `internal/server/handlers.go` is the one place that handles
    raw `json.Decoder` errors and deliberately does not surface
    `err.Error()` when the message could carry payload
    fragments.
  - Bridge errors collapse to 502/503 so upstream 4xx/5xx
    framing never reaches the caller. Full bridge body snippets
    are logged at the gateway, not returned.
  - Auth middleware never logs the submitted token (it logs
    scheme-rejection reasons and "invalid token" with the remote
    addr, no token material).
  - The bridge `Message` is constructed only from
    `AskRequest.Query`; `r.Header` is never read by the handler
    path that builds the bridge payload, so the bearer token
    cannot enter the model's context window.
  - Audit log omits query text and completion text by default.
  - `/metrics` endpoint is on a dedicated loopback-by-default
    listener; per-caller token counters are not cluster-reachable
    unless an operator explicitly opts in.
- *Residual risk:* when `OPENCOST_AI_AUDIT_LOG_QUERY` is enabled,
  query/completion text lands in stdout. Operators enabling this
  flag take responsibility for the resulting log retention.

**Denial of service**

- *Threat:* a caller exhausts gateway CPU, memory, or upstream
  Ollama capacity.
- *Mitigations:*
  - Per-caller token-bucket rate limiter
    (`internal/ratelimit`, default 60/min) keyed on the SHA-256
    prefix of the token. A noisy neighbour cannot starve other
    callers.
  - `ReadHeaderTimeout` and `ReadTimeout` caps on the public
    listener. The write timeout is intentionally zero on the
    streaming path (SSE is long-lived), so per-request deadlines
    live inside the handler instead: `internal/server/stream.go`
    refreshes an `http.ResponseController` write deadline
    (`OPENCOST_AI_REQUEST_TIMEOUT`) after every frame, so a client
    that stops draining its socket without closing it — which does
    *not* cancel the request context on its own — cannot pin the
    handler goroutine and the upstream bridge stream indefinitely.
    A long but actively-draining response is unaffected, since the
    deadline resets on every chunk of forward progress.
  - The non-streaming bridge client's per-call timeout is likewise
    `OPENCOST_AI_REQUEST_TIMEOUT` (`internal/bridge`, wired via
    `WithHTTPClient` in `cmd/gateway/main.go`), not a hardcoded
    constant independent of operator config.
  - `MaxBytesReader` ceiling per-request.
  - Envelope + query size ceilings prevent context-window
    exhaustion on the upstream model.
  - Tool names reported by the model are sanitised
    (`internal/server/handlers.go` `sanitizeToolNameLabel`) before
    they become a Prometheus label value, collapsing anything that
    isn't a plausible MCP tool name to one fixed `_invalid` series.
    Without this, an unbounded or prompt-injected tool name could
    grow the gateway process's own memory via
    `prometheus.CounterVec`/`HistogramVec`, which mint a permanent
    series per unique label value with no expiry — independent of
    whether `/metrics` itself is reachable.
- *Residual risk:* a distributed attack from many stolen tokens
  would spread across many buckets and amplify upstream load.
  Cluster-wide NetworkPolicy and issuance-side token controls
  are the documented mitigation; the rate limiter is per-caller
  by design.

**Elevation of privilege**

- *Threat:* a caller escapes bearer-token scope to reach admin
  surfaces (e.g. `/metrics`, Kubernetes API).
- *Mitigations:*
  - No admin surface is exposed on the public listener. `/metrics`
    is a separate listener, unauthenticated, loopback-by-default.
  - Gateway pod ServiceAccount has no RBAC bindings beyond what
    Kubernetes grants implicitly — it does not read/write the
    Kubernetes API.
  - No path on `/v1` accepts free-form commands. Every route has
    a typed handler; there is no tool-execution endpoint.
- *Residual risk:* none identified in v0.1.

### 4.2 Bridge client (`internal/bridge`)

**Spoofing**

- *Threat:* a malicious pod impersonates the bridge.
- *Mitigations:* NetworkPolicy on the gateway allows egress only
  to the configured bridge `Service`; a sibling pod in the same
  namespace cannot satisfy the egress rule unless it carries the
  bridge's label set.
- *Residual risk:* intra-namespace label spoofing is possible for
  anyone with CREATE pods permission in the namespace. Operators
  who need stricter guarantees should scope RBAC on the namespace
  or enable mTLS via a service mesh.

**Tampering**

- *Threat:* on-wire manipulation of chat responses or tool
  results; a compromised or MITM'd bridge issuing a redirect to
  silently forward the caller's query body somewhere else.
- *Mitigations:*
  - Same-cluster traffic over the pod network, which is typically
    CNI-encrypted or physically isolated.
  - Both HTTP clients built in `internal/bridge.New` refuse to
    follow redirects (`CheckRedirect` returns
    `http.ErrUseLastResponse`) — a bridge that starts issuing 3xx
    responses surfaces as an ordinary upstream error instead of
    silently forwarding the request elsewhere. The bridge is a
    single configured hop; there is no legitimate reason for either
    client to follow a redirect.
- *Residual risk:* traffic tampering by a CNI-layer attacker is
  out of scope for v0.1; in-cluster mTLS is a v0.2 consideration.

**Information disclosure**

- *Mitigations:*
  - `*bridge.Error` retains at most `maxErrorBodyBytes` (4 KiB) of
    upstream body snippet, and that snippet never reaches the
    caller (problem+json is composed from a fixed detail string).
  - Bridge request URLs are joined with a fixed, hardcoded endpoint
    literal (`"/api/chat"`, `"/api/tags"`) at every call site — no
    user or caller input ever reaches the path-join logic in
    `internal/bridge/client.go`, so there is no path-traversal
    vector today. That join is plain string concatenation, not
    `path.Clean`-style normalisation; if this code ever grows a
    caller-influenced path segment, add real cleaning at that point
    rather than relying on the current absence of dynamic input.

### 4.3 Audit log (`internal/audit`)

**Information disclosure**

- Query and completion text are **not** recorded when
  `OPENCOST_AI_AUDIT_LOG_QUERY=false` (default). The `Log`
  function unconditionally clears both fields before marshalling
  when the flag is off, so a caller passing them in anyway
  cannot leak them through the audit path.
- Caller identity is a 64-bit (16 hex chars) SHA-256 prefix of
  the bearer token. Reversing that to the raw token requires a
  pre-image attack even against a low-entropy token; the
  operator-recommended 32-byte random token makes the attack
  infeasible by construction.
- Tool *arguments* are deliberately dropped from the audit
  record (`internal/audit/audit.go` `ToolCall`), keeping only
  the tool name and duration. Arguments can carry filtering
  predicates over cost data that leak cluster shape.

**Tampering**

- The audit log writes to stdout, which Kubernetes streams to the
  node's log driver. Downstream integrity is the log shipper's
  responsibility; the gateway does not sign log lines in v0.1.

**Repudiation**

- See §4.1 "Repudiation." The audit stream is the primary
  repudiation-resistance control.

### 4.4 Auth token source (`internal/auth`)

**Spoofing**

- File-mtime-and-size watch reloads the token on rotation. If the
  file is rewritten atomically (the Kubernetes Secret mount does
  this), the old token stops working on the next request. Size is
  checked alongside mtime specifically so two rapid rotations
  (revoke-then-replace, done twice in quick succession after a
  leak) that happen to land on the same mtime tick on a coarse
  filesystem clock still trigger a reload, rather than the second,
  real rotation being missed until a later stat sees a
  distinguishable timestamp.
- On a transient stat failure the source holds the cached token
  rather than locking the gateway out — a missing file is more
  likely a race against an atomic rewrite than a revocation.

**Information disclosure**

- Token bytes are not logged. `Path()` exists for operator
  diagnostics and returns only the filesystem path, never the
  contents.
- Token bytes leave the package only via
  `subtle.ConstantTimeCompare`, which cannot timing-leak the
  contents given equal-length inputs.

**Residual risk**

- `subtle.ConstantTimeCompare` returns 0 immediately on length
  mismatch, so the *length* of the configured token is
  timing-observable. Operators provisioning a fixed-length random
  token (the documented posture) make this a non-issue; if a
  deployment uses a variable-length token this should be
  re-evaluated.

### 4.5 Helm chart (`deploy/helm/opencost-ai/`)

**Spoofing**

- NetworkPolicies render for every enabled component. The default
  gateway-ingress policy scopes to same-namespace pods; operators
  add `namespaceSelector`/`podSelector` entries for cross-
  namespace clients. An unset `allowedFrom` does **not** collapse
  to "any source" — the template injects `- podSelector: {}`
  (same-namespace only) as the floor, precisely to prevent
  accidental open ingress.

**Tampering / Information disclosure**

- `podSecurityContext` and container `securityContext` are set to
  PodSecurity `restricted`-compliant defaults (non-root 65532,
  read-only root FS, all capabilities dropped, RuntimeDefault
  seccomp). Overrides that weaken these land as chart-lint
  failures in the `--strict` CI profile before they reach a
  cluster.
- Chart does NOT render a bearer token. Operators either
  reference an existing Secret or pass `--set
  gateway.auth.token=...` at install time — the token never lives
  in a committed `values.yaml`.
- All three default image tags are pinned (gateway to
  `Chart.AppVersion`, bridge to the documented `v0.2.0`, Ollama to
  a specific release) — none defaults to `latest`. `image.digest`
  is available per-component as an opt-in for reproducible
  air-gap installs.

**Denial of service**

- Gateway and bridge Deployments have explicit resource requests
  and limits. Ollama's `StatefulSet` requests 2 Gi / limits 8 Gi
  by default; air-gap deployments with larger models must
  override.

**Elevation of privilege**

- Gateway ServiceAccount is bindings-free. It does not reach the
  Kubernetes API, and RBAC review should confirm that before
  each release.

### 4.6 Air-gap install flow (`scripts/air-gap/`, `docs/air-gap-install.md`)

**Spoofing / Tampering** (supply-chain)

- Image pulls go through `crane copy`, which preserves the
  manifest digest; operators pin `image.digest` in
  `values-airgap.yaml` after mirroring so the cluster never
  resolves by tag.
- Model weights ship as OCI artefacts via ORAS; the same
  `cosign verify` + registry policy the operator already uses
  for images applies.
- Helm chart tests render three value profiles and PodSecurity-
  label the integration namespace so restricted compliance is
  verified before release, not aspirational.

**Information disclosure**

- Air-gap test harness (`test/airgap/run.sh`) creates Secrets via
  `kubectl create secret --from-literal=token=…`. The workflow
  deliberately does **not** run under `bash -x` so the token
  value does not land in the public CI log.

**Denial of service / Elevation of privilege**

- Not applicable to the offline flow itself. The resulting
  cluster inherits the gateway's in-cluster controls (§4.1–§4.5).

### 4.7 Metrics endpoint

**Spoofing / Tampering** — N/A for Prometheus text exposition.

**Information disclosure**

- `/metrics` exposes per-caller rate-limit counters, per-model
  token totals, and per-tool invocation counts. An attacker
  scraping this endpoint learns usage patterns without ever
  touching `/v1/ask`.
- Default bind is `127.0.0.1:9090`. Cluster-wide exposure
  requires both a deliberate `OPENCOST_AI_METRICS_LISTEN_ADDR`
  override **and** a NetworkPolicy allowlist change. Both are
  operator opt-ins. In the Helm chart, the first opt-in is
  `gateway.config.metricsClusterAccessible` (default `false`) —
  the template only renders the override when this is explicitly
  set, so a default `helm install` keeps the loopback-only floor
  and `gateway.serviceMonitor.enabled=true` without it fails the
  template render rather than shipping a ServiceMonitor that can
  never successfully scrape.

## 5. Known risks the design accepts

- **Single bearer token shared by all callers.** v0.1 has no
  per-user identity; audit granularity is token-level.
  SPIFFE/SPIRE identity is the v0.2 fix.
- **No mTLS in-cluster.** Gateway ↔ bridge ↔ Ollama ↔ OpenCost
  traffic rides the pod network. Operators needing transport
  encryption deploy a service mesh today.
- **Prompt injection via MCP tool output.** Tool results share
  the model's context window with user messages. The system
  prompt (see `docs/prompts.md` §3.5) includes defence-in-depth
  language but does not eliminate the class.
- **Audit log tamper-evidence.** The log stream is stdout; a
  compromised node kubelet can drop lines before they reach a
  shipper. Line-signing is out of scope for v0.1.
- **Bridge is an upstream we do not own.** `jonigl/ollama-mcp-
  bridge` has its own threat surface; findings there are
  coordinated with the upstream maintainer and tracked in
  `SECURITY.md` scope.
- **Model licensing drift.** The default model
  (`granite4.2:8b`, Apache-2.0) and the documented Granite
  overrides are all permissively licensed today. An operator
  overriding to a non-permissive model (`llama3.1` variants
  under the Meta community license, for example) takes
  responsibility for that license fit.
- **Reasoning-trace exposure (Granite 4.2 thinking mode).** Every
  Granite 4.2 tag can emit a chain-of-thought before its answer,
  which the gateway forwards verbatim as a `thinking` SSE event
  (`internal/server/stream.go`). This is not logged to the audit
  trail (`audit.Event` has no `Thinking` field, unconditionally,
  regardless of `OPENCOST_AI_AUDIT_LOG_QUERY`) and it is only ever
  sent to the same authenticated caller who issued the query, so it
  does not cross a trust boundary the caller doesn't already sit
  inside. The residual risk is client-side: a UI that logs or
  screenshots the `thinking` stream captures cost data outside the
  gateway's own audit controls. Operators building clients on top of
  streaming `/v1/ask` should treat `thinking` events with the same
  handling care as `token` events.
- **Agentic-RL tool-use training (Granite 4.2, 8B/30B).** These tags
  were trained with reinforcement learning on tool use, code editing,
  and terminal/web-search actions inside sandboxed *training*
  environments. That training does not add any capability at
  inference time — the model can only invoke tools the bridge
  actually registers from the OpenCost MCP server, and `internal/
  bridge` has no code path that executes a model-proposed action
  outside that tool-call protocol. The practical effect is a model
  more inclined to *narrate or attempt* tool-shaped behaviour (e.g.
  proposing a shell command in prose) than 4.1 was; this is a prompt-
  quality question for `docs/prompts.md`, not a new attack surface,
  since there is nothing in the gateway that would execute such a
  proposal.

## 6. Operator checklist

When installing or auditing an `opencost-ai` deployment, verify:

- [ ] Bearer token Secret is created with at least 32 bytes of
      random data (`openssl rand -hex 32`), not a human-chosen
      value.
- [ ] Namespace is labelled
      `pod-security.kubernetes.io/enforce=restricted`.
- [ ] Image references use `image.digest` pinning, not floating
      tags, especially in air-gap installs.
- [ ] `networkPolicy.enabled=true` (the default) and the
      NetworkPolicies for all three components are present in
      the namespace.
- [ ] Prometheus scrape path scopes
      `networkPolicy.metricsIngress.allowedFrom` to the actual
      scraper pod/namespace; does not use `{}` (empty match-all).
- [ ] `OPENCOST_AI_AUDIT_LOG_QUERY` is `false` unless a
      documented compliance requirement says otherwise, with
      matching retention controls downstream.
- [ ] Gateway image signature has been verified with `cosign
      verify --certificate-identity-regexp ...
      --certificate-oidc-issuer ...` against the release's
      OIDC identity.
- [ ] SBOM (SPDX) from the release has been ingested into the
      operator's vulnerability scanner.
- [ ] SLSA provenance has been verified with
      `slsa-verifier verify-image` and the provenance's
      `builder.id` matches the project's release workflow
      identity.
- [ ] No custom values override `podSecurityContext`,
      `securityContext`, `runAsUser`, or
      `readOnlyRootFilesystem`.
- [ ] Release tag in use is within the supported window per
      `SECURITY.md`.

## 7. Reporting

Security issues: `SECURITY.md`. Non-security issues: the public
issue tracker. Do not put repro details for in-scope issues in a
public issue.
