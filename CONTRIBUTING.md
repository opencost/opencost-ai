# Contributing to opencost-ai

Thanks for your help improving opencost-ai. It is a CNCF sub-project
under [OpenCost](https://github.com/opencost/opencost) and follows
OpenCost's contributor conventions, plus the project-local rules
recorded in [`CLAUDE.md`](CLAUDE.md) and [`docs/architecture.md`](docs/architecture.md).

There are many ways to contribute:

* contributing or providing feedback on the OpenCost Spec
* contributing documentation here or to the [OpenCost website](https://github.com/opencost/opencost-website)
* joining the discussion in the [CNCF Slack](https://slack.cncf.io/) in the [#opencost](https://cloud-native.slack.com/archives/C03D56FPD4G) channel
* keeping up with community events using our [Calendar](https://bit.ly/opencost-calendar)
* participating in the fortnightly [OpenCost Working Group](https://bit.ly/opencost-calendar) meetings ([notes here](https://bit.ly/opencost-meeting))
* committing software via the workflow below

Before you start on anything non-trivial, read:

1. [`README.md`](README.md) — what this project is and is not.
2. [`docs/architecture.md`](docs/architecture.md) — intent, scope, and §10 resolved decisions (binding).
3. [`CLAUDE.md`](CLAUDE.md) — project "non-negotiables". They apply to human contributors too, not just AI-assisted sessions.

## Getting Help

If you have a question about opencost-ai or have encountered problems using it,
you can start by asking a question on [CNCF Slack](https://slack.cncf.io/) in the [#opencost](https://cloud-native.slack.com/archives/C03D56FPD4G) channel or attend the biweekly [OpenCost Working Group community meeting](https://bit.ly/opencost-meeting) from the [Community Calendar](https://bit.ly/opencost-calendar) to discuss opencost-ai development.

## Workflow

This repository's contribution workflow follows a typical open-source model:

- [Fork](https://docs.github.com/en/get-started/quickstart/fork-a-repo) this repository.
- Branch from `develop`. Branch naming: `<type>/<short-description>` where `<type>` is one of `feat`, `fix`, `docs`, `chore`, `security`, `refactor`, `test`. Examples: `feat/bridge-client`, `fix/audit-log-redaction`.
- Work on the forked repository.
- Open a pull request to [merge the fork back into this repository](https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/proposing-changes-to-your-work-with-pull-requests/creating-a-pull-request-from-a-fork) against `develop`. Do not target `main` — it holds released versions only (see [`.github/branch-protection.md`](.github/branch-protection.md)).
- Keep branches short-lived. Rebase, don't merge, to keep history linear.

## Building opencost-ai

Dependencies:

1. Current stable Go (1.26 as of initial commit), matching `go.mod`.
2. Docker (for building the distroless gateway image).
3. `helm` (for linting/templating the chart under `deploy/helm/opencost-ai`).
4. `kind` + `kubectl` (optional, for running the in-repo integration and air-gap end-to-end harnesses locally).

No `just`, no Tilt — plain `go` commands and `helm` suffice.

### Build the gateway

```bash
go build ./...
```

To build the container image:

```bash
docker build -t opencost-ai-gateway:dev .
```

The `Dockerfile` targets a distroless base and runs as UID 65532 with
a read-only root filesystem. If your change modifies the image, make
sure all three properties still hold (see the security checklist
below).

### Build the Helm chart

```bash
helm lint deploy/helm/opencost-ai
helm template release-name deploy/helm/opencost-ai > /tmp/default.yaml
```

The `ci/` sub-directory under the chart contains values profiles used
by CI (`lint-values.yaml`, `integration-values.yaml`). Use them to
exercise non-default code paths.

## Running locally

The gateway needs an `ollama-mcp-bridge` to talk to; for local
development the easiest loop is:

```bash
# Terminal 1: your bridge / Ollama stack
# (see docs/architecture.md §4 for the topology)

# Terminal 2: gateway
export OPENCOST_AI_BRIDGE_URL="http://127.0.0.1:8765"
export OPENCOST_AI_BEARER_TOKEN_FILE="/tmp/token"
head -c 32 /dev/urandom | base64 > /tmp/token
go run ./cmd/gateway
```

For the full air-gap install flow (kind cluster, in-cluster registry,
iptables egress block) see [`docs/air-gap-install.md`](docs/air-gap-install.md)
and `test/airgap/run.sh`.

## Code Formatting

Before submitting a pull request, ensure your code is properly formatted:

```bash
# Format all Go code
go fmt ./...
```

To check if your code is formatted without making changes:

```bash
gofmt -l .
```

The CI pipeline will automatically check code formatting on pull requests.

## Testing code

Testing is provided by `go test`:

```bash
go test -race -count=1 ./...
```

This runs unit tests with the race detector under a fresh cache on
every invocation. `go vet ./...`, `staticcheck ./...`, and
`govulncheck ./...` are all enforced in CI as well.

### Running the integration tests

Integration tests live under `test/integration/` behind a build tag:

```bash
go test -tags=integration -race ./test/integration/...
```

The Helm-chart and air-gap end-to-end harnesses live under
`.github/workflows/helm.yml`, `.github/workflows/airgap-e2e.yml`, and
`test/airgap/run.sh`. They require `kind`, `kubectl`, `helm`, and —
for the air-gap harness — `iptables` and root privilege on a Linux
host.

## Code Review Standards

All pull requests must be reviewed before merging. The review process ensures:

### What reviewers check:
- **Correctness:** Does the code do what it claims?
- **Tests:** Are new features and bug fixes covered by tests?
- **Style:** Does the code follow Go conventions (`gofmt`, `go vet`)?
- **Security:** Are inputs validated? Are credentials handled safely?
- **Performance:** Are there obvious performance issues (unbounded allocations, N+1 queries)?

### Review requirements:
- At least one approval from a Committer or Maintainer is required
- The reviewer must be a different person than the PR author
- For security-sensitive changes, review by a Maintainer is required
- Emergency fixes may bypass review with post-merge review required within 48 hours (per [GOVERNANCE.md](GOVERNANCE.md))

## Regression Tests

When fixing a bug, contributors SHOULD add a test that reproduces the bug before applying the fix. This ensures the bug does not recur. As a project-wide goal, at least 50% of bugs fixed in any six-month window should have corresponding regression tests. This is tracked by maintainers using issues labeled `bug` and measured during release reviews; it is an aspirational target for the project as a whole, not a requirement applied to individual contributors.

## Finding Issues to Work On

Look for issues labeled [`good first issue`](https://github.com/opencost/opencost-ai/labels/good%20first%20issue) or [`help wanted`](https://github.com/opencost/opencost-ai/labels/help%20wanted) for a curated list of tasks suitable for new contributors.

## Certificate of Origin

By contributing to this project, you certify that your contribution was created in whole or in part by you and that you have the right to submit it under the open source license indicated in the project. In other words, please confirm that you, as a contributor, have the legal right to make the contribution. This is enforced on Pull Requests and requires `Signed-off-by` with the email address for the author in the commit message.

Use `git commit -s` to add the trailer automatically. GPG signing is
*not* required (matching upstream OpenCost); the DCO sign-off is the
gate. The [DCO GitHub App](https://github.com/apps/dco) runs on every
PR and blocks merge on a missing or mismatched sign-off.

## Committing

Please write a commit message with `Fixes #<issue>` if there is an
outstanding issue that is fixed. It's okay to submit a PR without a
corresponding issue; just please try to be detailed in the description
of the problem you're addressing.

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/).
Subject line ≤ 72 chars, imperative mood, lower-case after the type.
The body explains *why*, not *what*.

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

**Code Formatting:** All code must be formatted with `go fmt ./...`
before submitting. The CI pipeline will reject PRs with unformatted
code.

**Code Quality:** While lint warnings are acceptable in some cases
(e.g., comments on exported functions are nice but not strictly
required), please address any critical issues reported by `go vet`
and `staticcheck`.

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

The CI workflow (`.github/workflows/authorship.yml`) and the DCO
GitHub App are authoritative — the local hooks exist to make the
mistake cheap to fix (amend locally) instead of expensive (force-push
a rewrite). An `Assisted-by: Claude Code` trailer is welcome and does
not trip either check; it supplements the human `Signed-off-by:`.

## Reporting bugs and security issues

- **Security issues:** follow [`SECURITY.md`](SECURITY.md). Do not
  open a public issue.
- **Bugs and enhancements:** open a GitHub issue using the templates
  under `.github/ISSUE_TEMPLATE/`. Include the commit SHA and
  environment details.

## When in doubt

Stop and ask — in a draft PR, a GitHub discussion, or the OpenCost
Slack. [`docs/architecture.md`](docs/architecture.md) §10 records
settled decisions; the rest is open for proposal.
