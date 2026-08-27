# Changelog

All notable changes to `opencost-ai` are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/).

Prompt revisions (per `docs/prompts.md` §6) are recorded under the
gateway release that ships them, referencing the prompt version tag.

## [Unreleased]

### Changed

- Default model bumped from `granite4.1:8b` to `granite4.2:8b`
  (IBM Granite 4.2, released 2026-08-25). Same size lineup
  (3B/8B/30B) and Apache 2.0 licence as 4.1; all model references in
  code, tests, Helm defaults, and docs updated accordingly. See
  `docs/architecture.md` §10 decision 6 and `docs/security.md` §5
  for the accompanying security review of the new switchable
  thinking mode and agentic-RL tool-use training on the 8B/30B tags.

### Security

- Reviewed the Granite 4.1 → 4.2 model bump: no new attack surface
  identified (reasoning traces stay outside the audit log exactly as
  4.1's already did; agentic-RL training does not grant the model
  any tool the bridge doesn't itself register). Flagged one item for
  follow-up before release: the documented `ollama` ≥ 0.30.8 floor
  in `docs/air-gap-install.md` has not been re-verified against
  Granite 4.2 specifically.
- Full-codebase secops review (application code, Kubernetes/Helm
  deployment, CI/CD supply chain) with every finding fixed in the
  same pass, verified against source before and after:
  - **Gateway:** `OPENCOST_AI_REQUEST_TIMEOUT` now actually bounds
    the non-streaming bridge call and, via a refreshed
    `http.ResponseController` write deadline, how long a
    connected-but-not-draining SSE client can pin a handler
    goroutine and the upstream stream — previously neither path
    enforced it despite the config doc comment claiming otherwise.
    Model-reported tool names are sanitised before becoming a
    Prometheus label, closing an unbounded-cardinality memory-growth
    path. The bridge HTTP clients no longer follow redirects. The
    auth token source now checks size alongside mtime so two rapid
    rotations landing on the same filesystem-clock tick are no
    longer indistinguishable from "unchanged."
  - **Helm chart:** the gateway's loopback-only metrics default is
    no longer unconditionally overridden by the chart — cluster
    accessibility is now the explicit `gateway.config.
    metricsClusterAccessible` opt-in the threat model already
    described, and enabling `gateway.serviceMonitor` without it now
    fails the template render instead of shipping a scrape target
    that can never succeed. The equivalent NetworkPolicy rule gained
    the same empty-allowlist guard the sibling ingress rule already
    had. The bridge image no longer defaults to `latest`. The
    distroless runtime base is now digest-pinned and tracked by a
    new Dependabot `docker` ecosystem entry.
  - **CI/CD:** six actions in the release signing/attestation
    pipeline, plus `codeql.yml` and `vulnerability-scan.yml`'s SARIF
    upload, are now SHA-pinned — closing gaps ranging from
    already-flagged TODOs to unpinned GitHub-generated boilerplate.
    Added the `scorecard.yml` workflow that `CHANGELOG.md`,
    `SECURITY.md`, and branch protection already referenced but
    which never existed. Fixed `sonarqube.yml` (wrong trigger branch,
    no checkout step, blank project key — it could not previously
    have run a scan). `helm.yml`'s Helm/kind installs are now
    checksum-verified, matching the pattern already used in
    `airgap-e2e.yml`. `authorship.yml`'s forbidden-author-name check
    now matches by prefix instead of exact string, so it actually
    catches the co-authorship convention this project's own
    `CLAUDE.md` documents. `export-gguf.sh` validates a manifest
    digest's shape before using it in a filesystem path. Corrected a
    mislabeled `actions/upload-artifact` version comment (an
    immutable, correct SHA — just a misleading comment next to it)
    in `ci.yml` and `helm.yml`.
  - `docs/security.md` updated throughout to match the code rather
    than describe the intended-but-unenforced design.

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
- Retained the existing `CODEOWNERS` file and now reference the
  `@opencost/opencost-ai-maintainers` GitHub team from it. Creating
  that team is a post-merge operator step; until it exists,
  "Require review from Code Owners" on `develop` will match no
  reviewer and block merges by design.

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
