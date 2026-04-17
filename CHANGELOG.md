# Changelog

All notable changes to `opencost-ai` are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/).

Prompt revisions (per `docs/prompts.md` §6) are recorded under the
gateway release that ships them, referencing the prompt version tag.

## [Unreleased]

### Governance

- Adopted upstream opencost's branch model: `develop` is the
  integration branch, `main` holds released-version pointers only,
  and release branches are named `v<MAJOR>.<MINOR>`. Existing CI
  workflows (`ci.yml`, `helm.yml`, `airgap-e2e.yml`, `authorship.yml`,
  `codeql.yml`) now trigger on `develop`. `release.yml` now sources
  the release branch derived from the tag instead of the tag's
  detached commit.
- Added governance files mirrored from upstream opencost with project
  name adjustments: `GOVERNANCE.md`, `MAINTAINERS.md`,
  `CODE_OF_CONDUCT.md`. `CONTRIBUTING.md` and `SECURITY.md` rewritten
  to match upstream's shape while retaining project-local build and
  run instructions. GPG signing is no longer required; DCO sign-off
  remains the gate.
- Added `.github/PULL_REQUEST_TEMPLATE.md`, issue templates under
  `.github/ISSUE_TEMPLATE/`, and `.github/dependabot.yml` (weekly
  `gomod` at `/` and weekly `github-actions` at `/`).
- Added `.github/workflows/scorecard.yml` (OSSF Scorecard) and
  `.github/workflows/vulnerability-scan.yml` (Trivy filesystem
  scan, fails on CRITICAL/HIGH). Both publish SARIF to the Security
  tab. DCO enforcement is delegated to the DCO GitHub App; the
  install step is a post-merge operator action.
- Added `.github/branch-protection.md` documenting the exact branch
  protection and tag protection rules a maintainer must apply to
  `develop`, `main`, and `v*` release branches.

## v0.1.0 — 2026-04-17

First tagged release. Establishes the v0.1 contract: a small Go HTTP
gateway in front of `jonigl/ollama-mcp-bridge`, shipped as a signed
distroless image with a Helm chart, an air-gap install flow, and the
supply-chain artefacts (cosign signature, SPDX SBOM, SLSA v1.0
provenance) required by `SECURITY.md` and `docs/security.md`.

### Added

**Gateway binary (`cmd/gateway`)**

- Non-streaming `POST /v1/ask` with RFC 7807 problem+json error
  responses, 4 KiB query ceiling, 8 KiB envelope ceiling, strict
  JSON decoding (`DisallowUnknownFields` + exactly-one-value
  check), UTF-8 and UUID validation.
- Streaming `POST /v1/ask` (SSE) emitting `thinking`, `tool_call`,
  `tool_result`, `token`, `done`, and `error` events with the
  contract documented in `docs/api.md` §4.2.2.
- `GET /v1/tools` — returns an empty list with
  `discovery_deferred:true` because the bridge does not yet expose
  tool listing (see `docs/architecture.md` §11.5).
- `GET /v1/models` — proxies `/api/tags` through the bridge.
- `GET /v1/health` — liveness-only, unauthenticated, returns build
  version. Kubernetes readiness probes must not target this
  endpoint until `/v1/ready` lands.
- `GET /metrics` on a separate loopback-default listener.
  Pre-registered Prometheus series for request counters,
  latencies, in-flight gauge, tool calls, tool duration, model
  tokens, upstream errors, and rate-limit rejections.
- Bearer-token authentication with mtime-watched token file
  reload, constant-time comparison, RFC 6750 `WWW-Authenticate`
  on 401.
- Per-caller token-bucket rate limit keyed on the SHA-256 prefix
  of the bearer token.
- Structured JSON audit log (`internal/audit`) recording request
  ID, caller identity, model, token counts, tool calls, latency,
  status, and outcome. Query and completion text remain off by
  default.
- Per-request `X-Request-ID` correlation (`internal/requestid`),
  honoured from the caller when it passes sanitisation.
- Graceful shutdown on SIGTERM/SIGINT draining both listeners.

**Supply chain and operations**

- Distroless (`gcr.io/distroless/static-debian12:nonroot`) image
  running as UID 65532 with read-only root filesystem.
- Helm chart (`deploy/helm/opencost-ai`) shipping the gateway,
  `ollama-mcp-bridge`, and Ollama with a PVC for the model cache.
  NetworkPolicies scope egress to the minimum topology; pods run
  under PodSecurity `restricted`.
- Air-gap install flow documented end-to-end in
  `docs/air-gap-install.md` and validated by a CI harness that
  blocks public internet egress with `iptables` rules
  (`test/airgap/`, `.github/workflows/airgap-e2e.yml`).
- `scripts/air-gap/` — ORAS export, push, and pull for model
  weights as OCI artefacts; `crane` image mirroring.
- Release workflow (`.github/workflows/release.yml`) producing,
  per `v*.*.*` tag:
  - Multi-arch (`linux/amd64,linux/arm64`) image pushed to
    `ghcr.io/opencost/opencost-ai-gateway` with tag, full, minor,
    and major references (no `latest`).
  - Cosign keyless signature bound to the release workflow's
    OIDC identity.
  - SPDX SBOM attested to the image and attached as a release
    asset.
  - SLSA v1.0 provenance generated via the official
    `slsa-framework/slsa-github-generator` reusable workflow.
  - Packaged Helm chart attached to the GitHub release with a
    tag-vs-Chart.yaml version-match gate.

**Docs**

- `docs/architecture.md` — intent and target architecture,
  resolved decisions, and a new §11 recording the delta between
  the spec and what shipped in v0.1.0.
- `docs/api.md` — operator-facing HTTP reference for every
  `/v1` route, error mapping, rate-limit semantics, and worked
  examples.
- `docs/prompts.md` — the intended system prompt text, a
  paragraph-by-paragraph rationale, and documentation that the
  gateway does not inject it in v0.1 (v0.2 work).
- `docs/security.md` — STRIDE threat model for every in-scope
  component, enumerated accepted risks, and an operator audit
  checklist for installs.
- `docs/air-gap-install.md` — end-to-end offline install flow.

### Known gaps (tracked for v0.2)

- `internal/prompt` system-prompt injection (`docs/prompts.md`
  §1, architecture §11.3).
- `AskRequest.format="json"` structured responses (architecture
  §11.4).
- `/v1/tools` live discovery pending bridge support
  (architecture §11.5).
- `/v1/ready` upstream-reachability probe (architecture §11.6).
- OTLP tracing (architecture §11.9).
- SPIFFE/SPIRE workload identity replacing the static bearer
  token (architecture §7.3, security §5).

### Security

- All non-negotiables from `CLAUDE.md` are enforced: non-root
  container, read-only root filesystem, no internet egress, no
  raw exception strings in responses, query text off by default
  in the audit log.
- Dependencies pinned to exact versions; `go mod verify` runs in
  CI and the Dockerfile build. `govulncheck` runs per push and
  uploads a JSON report as a CI artefact.
- Helm chart integration test labels the namespace
  `pod-security.kubernetes.io/enforce=restricted` before
  installing, so restricted compliance is verified rather than
  assumed.

### Prompt

- `opencost-ai-prompt/v0.1` defined in `docs/prompts.md`; not
  injected at runtime in v0.1.0. Operators who want the
  guardrail today apply it client-side or via the bridge's
  `system_prompt` configuration.

[Unreleased]: https://github.com/opencost/opencost-ai/compare/v0.1.0...HEAD
