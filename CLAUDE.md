# CLAUDE.md

Instructions for Claude Code sessions on this repository.

## Project

`opencost-ai` is a Kubernetes-native, air-gap-deployable, open-source AI
assistant for OpenCost. It exposes a small, stable HTTP API in front of
`jonigl/ollama-mcp-bridge`, which in turn connects a local Ollama runtime
to the OpenCost MCP server (built into OpenCost v1.118+).

Full architecture and rationale: `docs/architecture.md`. Read it before
making any non-trivial change.

## Non-negotiables

Refusal conditions, not preferences. If a requested change violates one
of these, stop and ask.

- **Never commit secrets.** Token files, kubeconfigs, model weights,
  SBOMs with build metadata, `.env`, private keys. Check `.gitignore`
  before staging.
- **Commits must be DCO signed-off and GPG signed.** `git commit -s -S`.
  This repo is under the OpenCost CNCF project. See the
  `opencost-contributor` skill for the workflow.
- **Gateway runs as non-root.** UID 65532, read-only root filesystem. No
  `hostPath`, no `hostNetwork`, no `privileged`. Any Dockerfile or Helm
  change that weakens this is a blocker.
- **Never log query text or model completion by default.** Both may
  contain sensitive cost data. Audit log includes request ID, caller
  identity, model, token counts, tool calls — not content. The opt-in
  `OPENCOST_AI_AUDIT_LOG_QUERY` flag exists; do not flip its default.
- **Never return raw exception strings to clients.** Use RFC 7807
  problem+json. Map internal errors to stable, documented codes.
- **Do not reintroduce the TUI-scraping approach from `legacy/`.** The
  Flask + `pexpect` prototype is preserved for archaeological reasons.
  All new code talks to `ollama-mcp-bridge` via its HTTP API.

## Code style and structure

- **Language:** Go 1.22+ for the gateway. Shell only for CI glue.
- **Dependencies:** prefer the standard library. New third-party deps in
  `internal/` require a justification comment on the import and a note
  in the PR description.
- **LOC budget:** gateway (everything under `cmd/` + `internal/` +
  `pkg/`) stays under 2000 lines. Push back on scope if a feature
  threatens this.
- **Package boundaries:**
  - `cmd/gateway` — `main.go` only. Wire-up, no logic.
  - `internal/server` — HTTP handlers, middleware, routing.
  - `internal/bridge` — client for `ollama-mcp-bridge`. Nothing else
    talks to the bridge.
  - `internal/auth`, `internal/audit`, `internal/ratelimit`,
    `internal/prompt`, `internal/config` — one concern each.
  - `pkg/apiv1` — exported types only. No behavior. Breaking changes
    require a version bump.
- **Errors:** always wrap with `fmt.Errorf("context: %w", err)`. Never
  swallow. Never `panic` in request paths.
- **Context:** every function that does I/O takes `context.Context` as
  the first parameter. No background goroutines without a tied context.
- **Tests:** table-driven, `_test.go` next to code. Integration tests in
  `test/integration/` gated behind a build tag.

## Security checklist before opening a PR

- [ ] No new dependencies, or new deps justified in PR description
- [ ] No hardcoded URLs, tokens, ports — all via config
- [ ] Input validated: length, content-type, required fields
- [ ] Errors mapped to problem+json — no raw `err.Error()` in responses
- [ ] New HTTP handlers have auth middleware applied
- [ ] New endpoints documented in `docs/api.md` and reflected in `pkg/apiv1`
- [ ] `go vet`, `staticcheck`, `govulncheck` all pass
- [ ] Tests added for happy path and at least one failure path
- [ ] If Dockerfile changed: still distroless, still UID 65532, still
      read-only root

## Design doc vs. code

`docs/architecture.md` is the source of truth for *intent*. If the code
diverges, the code is wrong. If the intent is wrong, update the design
doc in the same PR that changes the code and flag the reviewer.

## When you're unsure

Stop. Ask. Do not guess about: MCP transport choices, model licensing,
CNCF project conventions, Helm chart home (this repo vs.
`opencost-helm-chart`). These are open questions parked in
`docs/architecture.md` §10; they should not be silently answered in code.

## Repository conventions

- Branches: `feat/<short>`, `fix/<short>`, `docs/<short>`, `chore/<short>`.
- Commit messages: Conventional Commits. Signed-off-by line required.
- PR titles mirror commit style.
- Releases cut from `main` via `v*` tags. Images cosign-signed, SBOMs
  published, SLSA provenance generated.
