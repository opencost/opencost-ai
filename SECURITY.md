# opencost-ai Security Policy

The opencost-ai project greatly appreciates the need for security and
timely updates, given its proximity to cost data and the LLM tool-use
surface. We are very grateful to the users, security researchers, and
developers for reporting security vulnerabilities to us. All reported
security vulnerabilities will be carefully assessed, addressed, and
responded to.

## Code Security

Application code is version controlled using GitHub. All code changes
are tracked with full revision history and are attributable to a
specific individual. Code must be reviewed and accepted by a different
engineer than the author of the change. See
[`GOVERNANCE.md`](GOVERNANCE.md) for the review and approval rules and
[`.github/branch-protection.md`](.github/branch-protection.md) for the
required status checks on the `develop`, `main`, and release
(`v<MAJOR>.<MINOR>`) branches.

### Dependabot

opencost-ai has [Dependabot](https://docs.github.com/en/code-security/supply-chain-security/understanding-your-software-supply-chain/about-supply-chain-security#what-is-dependabot)
enabled for assessing dependencies in the project. See
[`.github/dependabot.yml`](.github/dependabot.yml).

Dependabot is complemented by:

- `govulncheck` (Go module graph) on every PR and push, from
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml).
- Trivy filesystem scan (container layer coverage) on every PR to
  `develop` and push to `develop`, from
  [`.github/workflows/vulnerability-scan.yml`](.github/workflows/vulnerability-scan.yml).
- OSSF Scorecard on PRs to `develop`, scheduled weekly, and on
  branch-protection-rule events, from
  [`.github/workflows/scorecard.yml`](.github/workflows/scorecard.yml).
- CodeQL static analysis from
  [`.github/workflows/codeql.yml`](.github/workflows/codeql.yml).

## Supported Versions

opencost-ai provides security updates for the two most recent minor
versions released on GitHub.

For example, if `v0.3.0` is the most recent stable version, we will
address security updates for `v0.2.0` and later. Once `v0.4.0` is
released, we will no longer provide updates for `v0.2.x` releases.

Until `v0.2.0` ships, `main` and the `v0.1.x` line are supported on a
best-effort basis.

## Reporting a Vulnerability

The opencost-ai project has enabled [Private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
for our repository which allows for direct reporting of issues to
administrators and maintainers in a secure fashion. Please include a
thorough description of the issue, the steps you took to create the
issue, affected versions, and, if known, mitigations for the issue.
The team will help diagnose the severity of the issue and determine
how to address the issue. Issues deemed to be non-critical will be
filed as GitHub issues. Critical issues will receive immediate
attention and be fixed as quickly as possible.

Report at
<https://github.com/opencost/opencost-ai/security/advisories/new>.

## Disclosure policy

For known public security vulnerabilities, we will disclose the
disclosure as soon as possible after receiving the report.
Vulnerabilities discovered for the first time will be disclosed in
accordance with the following process:

1. The received security vulnerability report shall be handed over to
   the security team for follow-up coordination and repair work.
2. After the vulnerability is confirmed, we will create a draft
   Security Advisory on GitHub that lists the details of the
   vulnerability.
3. Invite related personnel to discuss the fix.
4. Fork the temporary private repository on GitHub, and collaborate
   to fix the vulnerability.
5. After the fixed code is merged into all supported versions, the
   vulnerability will be publicly posted in the GitHub Advisory
   Database.

## Scope

In scope:

- The `opencost-ai-gateway` binary (`cmd/gateway`, `internal/`,
  `pkg/apiv1`).
- The Helm chart under `deploy/helm/opencost-ai/`.
- The container image published from this repository.
- Documentation that could mislead an operator into an insecure
  configuration.

Out of scope:

- Upstream `ollama-mcp-bridge`, `ollama`, and OpenCost itself.
  Report those to their respective projects. We will coordinate if an
  issue spans projects.
- The archived prototype under `legacy/prototype-flask/`. It is
  frozen, unbuilt, and not shipped. Issues there are acknowledged but
  will not be fixed.
- Findings that require the operator to disable shipped security
  controls (non-root UID, read-only root fs, auth, NetworkPolicy).

## Security properties we commit to

Per [`docs/architecture.md`](docs/architecture.md) §7.5 and
[`CLAUDE.md`](CLAUDE.md) "Non-negotiables":

- Gateway runs as non-root UID 65532 with a read-only root
  filesystem. No `hostPath`, no `hostNetwork`, no `privileged`.
- All `/v1/*` endpoints require authentication. v0.1 uses a static
  bearer token sourced from a Kubernetes Secret; v0.2 moves to
  SPIFFE/SPIRE workload identity.
- Errors returned to clients are RFC 7807 `problem+json` with stable,
  documented codes. Raw exception strings are never returned.
- The audit log records request ID, caller identity, timestamp,
  model, token counts, and tool calls. It does **not** record query
  text or completion text by default. The opt-in
  `OPENCOST_AI_AUDIT_LOG_QUERY` flag exists for deployments that need
  content logging; its default is off.
- Inputs are length-validated, content-type-checked, and rejected on
  unexpected fields.
- Dependencies are pinned to exact versions. `go mod verify` runs
  after every dependency change. `govulncheck` and Trivy run in CI.
- Release images are signed with `cosign`. SBOMs (SPDX) and SLSA
  provenance are published per release from
  [`.github/workflows/release.yml`](.github/workflows/release.yml).

## Hardening guidance for operators

[`docs/security.md`](docs/security.md) (STRIDE threat model + operator
audit checklist) and [`docs/air-gap-install.md`](docs/air-gap-install.md)
cover:

- Running in a namespace with PodSecurity `restricted` enforced.
- The NetworkPolicy shipped in the Helm chart (egress only to the
  bridge, Ollama, and OpenCost MCP; no internet).
- Rotating the bearer token by updating the backing Secret.
- Verifying image signatures with `cosign verify`.
- Verifying the SBOM and SLSA provenance of a pulled image.
