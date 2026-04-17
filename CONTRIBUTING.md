# Contributing to opencost-ai

Thanks for your interest. `opencost-ai` is an OpenCost CNCF sub-project
and follows OpenCost's contributor conventions, plus the project-local
rules recorded in `CLAUDE.md` and `docs/architecture.md`.

Before you start on anything non-trivial, read:

1. `README.md` — what this project is and is not.
2. `docs/architecture.md` — intent, scope, and §10 resolved
   decisions (binding).
3. `CLAUDE.md` — the project "non-negotiables". They apply to human
   contributors too, not just AI-assisted sessions.

## Code of conduct

This project follows the
[CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).
Report violations per that document.

## Licensing and DCO

- The project is Apache-2.0 (`LICENSE`). Contributions are accepted
  under the same license.
- **Every commit must carry a `Signed-off-by:` trailer** asserting
  the [Developer Certificate of Origin](https://developercertificate.org/).
  Use `git commit -s`.
- **Every commit must be cryptographically signed** (GPG or SSH).
  Use `git commit -S`, or configure `commit.gpgsign = true`.
- Combined: `git commit -s -S`. PRs with unsigned or un-signed-off
  commits will not be merged.

## Branches

- Branch from `main`.
- Branch naming: `<type>/<short-description>` where `<type>` is one
  of `feat`, `fix`, `docs`, `chore`, `security`, `refactor`, `test`.
  Examples: `feat/bridge-client`, `fix/audit-log-redaction`.
- Keep branches short-lived. Rebase, don't merge, to keep history
  linear.

## Commit messages

Conventional Commits. Subject line ≤ 72 chars, imperative mood,
lower-case after the type. The body explains *why*, not *what*.

```
<type>(<scope>): <subject>

<body — why this change, tradeoffs considered, links to issues>

Signed-off-by: Your Name <you@example.com>
```

Types: `feat`, `fix`, `docs`, `chore`, `security`, `refactor`, `test`,
`ci`, `build`. Use `security:` for any change with a security
justification, so the release notes can surface them.

If the work was assisted by a coding assistant, add an
`Assisted-by:` trailer naming the tool, per the Linux kernel
convention. The human `Signed-off-by:` is still required and still
carries DCO responsibility.

## Pull requests

1. One logical change per PR. Split mechanical refactors from
   behavior changes.
2. PR title mirrors the commit subject format.
3. Fill in the PR description: what changed, why, how it was tested,
   and any follow-up work deferred.
4. Link the issue if one exists.
5. Do not open a PR that weakens security posture (running as root,
   binding without auth, returning raw errors, logging query text
   by default). CLAUDE.md "Non-negotiables" are refusal conditions.

## Pre-submit checklist

Mirror of the CLAUDE.md security checklist; every PR author runs
through it before requesting review:

- [ ] No new dependencies, or new deps justified in the PR
      description with a supply-chain note.
- [ ] Dependencies pinned to exact versions; `go mod verify` run
      after dependency changes.
- [ ] No hardcoded URLs, tokens, ports — all via config.
- [ ] Inputs validated: length, content-type, required fields.
- [ ] Errors mapped to `problem+json` (RFC 7807) — no raw
      `err.Error()` in responses.
- [ ] New HTTP handlers have auth middleware applied.
- [ ] New endpoints documented in `docs/api.md` and reflected in
      `pkg/apiv1`.
- [ ] `go vet`, `staticcheck`, `govulncheck` all pass.
- [ ] Tests added: happy path + at least one failure path.
- [ ] If the Dockerfile changed: still distroless, still UID 65532,
      still read-only rootfs.
- [ ] If `docs/architecture.md` intent has changed, design doc is
      updated in the same PR.

## Code style

- **Current stable Go (1.26 as of initial commit).** Shell only for CI
  glue. No new Python in-tree.
- Prefer the standard library. New third-party deps in `internal/`
  need a justification comment on the import and a note in the PR
  description.
- LOC budget: gateway (`cmd/` + `internal/` + `pkg/`) under 2000
  lines. Push back on scope, not the budget.
- Package boundaries per `CLAUDE.md`:
  `cmd/gateway` → wire-up only; `internal/bridge` → only thing that
  talks to `ollama-mcp-bridge`; `pkg/apiv1` → exported types, no
  behavior.
- Every I/O function takes `context.Context` as the first argument.
- Wrap errors with `fmt.Errorf("context: %w", err)`. Never swallow.
  Never `panic` in request paths.
- Tests are table-driven and live next to the code. Integration
  tests live under `test/integration/` behind a build tag.

## Local hooks

The repo ships client-side hooks under `.githooks/` that mirror the
CI gates so the most common mistakes are caught before push:

- `pre-commit` rejects commits whose author name is `Claude` /
  `Anthropic` or whose author email is `noreply@anthropic.com`.
- `commit-msg` rejects commit messages whose `Signed-off-by:`
  trailer names a coding-assistant identity.

Opt in once per clone:

```sh
git config core.hooksPath .githooks
```

The CI workflow (`.github/workflows/authorship.yml`) is
authoritative — the local hooks exist to make the mistake cheap to
fix (amend locally) instead of expensive (force-push a rewrite).
An `Assisted-by: Claude Code` trailer is welcome and does not trip
either check; it supplements the human `Signed-off-by:`.

## Running locally

v0.1 scaffold is in progress; `docs/dev-setup.md` will land with the
first `cmd/gateway/main.go` commit. Until then the repo is
documentation + archived prototype only.

## Reporting bugs and security issues

- **Security issues:** follow `SECURITY.md`. Do not open a public
  issue.
- **Bugs and enhancements:** open a GitHub issue with a reproducible
  description. Include the commit SHA and environment details.

## When in doubt

Stop and ask — in a draft PR, a GitHub discussion, or the OpenCost
Slack. `docs/architecture.md` §10 records settled decisions; the
rest is open for proposal.
