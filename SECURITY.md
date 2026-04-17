# Security policy

`opencost-ai` is under the OpenCost CNCF organization. Security
reports are welcome against `main` and any tagged release inside
the support window below.

## Supported versions

| Version     | Supported         |
|-------------|-------------------|
| `main`      | Yes (best effort) |
| `v0.1.x`    | Yes               |
| `< v0.1.0`  | No                |

Once `v0.2.0` ships, the two most recent minor versions will
receive security fixes; this table will be updated at that point.

## Reporting a vulnerability

**Do not open a public GitHub issue for security reports.**

Report privately via GitHub Security Advisories:

1. Go to
   <https://github.com/opencost/opencost-ai/security/advisories/new>.
2. Describe the issue, the affected commit or tag, reproduction
   steps, and impact.
3. If you have a suggested fix, include it.

If GitHub Security Advisories is not available to you, email the
maintainers listed in `CODEOWNERS`. Please encrypt sensitive
contents if you can; if not, send a minimal report and we will set
up a secure channel.

## What to expect

- **Acknowledgement within 3 business days** of report receipt.
- **Triage within 10 business days** with a severity classification
  (CVSS v3.1) and a proposed timeline.
- **Fix target:** 30 days for high/critical, 90 days for medium, best
  effort for low. Complex issues may take longer; we will keep you
  updated.
- **Coordinated disclosure.** We will agree an embargo date with you
  before publishing an advisory. Credit is given in the advisory
  unless you ask otherwise.

## Scope

In scope:

- The `opencost-ai-gateway` binary (`cmd/gateway`, `internal/`,
  `pkg/apiv1`).
- The Helm chart under `deploy/helm/opencost-ai/` once it lands.
- The container image published from this repo.
- Documentation that could mislead an operator into an insecure
  configuration.

Out of scope:

- Upstream `ollama-mcp-bridge`, `ollama`, and OpenCost itself.
  Report those to their respective projects. We will coordinate if
  an issue spans projects.
- The archived prototype under `legacy/prototype-flask/`. It is
  frozen, unbuilt, and not shipped. Issues there are acknowledged
  but will not be fixed.
- Findings that require the operator to disable shipped security
  controls (non-root UID, read-only root fs, auth, NetworkPolicy).

## Security properties we commit to

Per `docs/architecture.md` §7.5 and `CLAUDE.md` "Non-negotiables":

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
  `OPENCOST_AI_AUDIT_LOG_QUERY` flag exists for deployments that
  need content logging; its default is off.
- Inputs are length-validated, content-type-checked, and rejected on
  unexpected fields.
- Dependencies are pinned to exact versions. `go mod verify` runs
  after every dependency change. `govulncheck` runs in CI.
- Release images are signed with `cosign`. SBOMs (SPDX) and SLSA
  provenance are published per release.

## Hardening guidance for operators

`docs/security.md` (STRIDE threat model + operator audit checklist)
and `docs/air-gap-install.md` cover:

- Running in a namespace with PodSecurity `restricted` enforced.
- The NetworkPolicy shipped in the Helm chart (egress only to the
  bridge, Ollama, and OpenCost MCP; no internet).
- Rotating the bearer token by updating the backing Secret.
- Verifying image signatures with `cosign verify`.
- Verifying the SBOM and SLSA provenance of a pulled image.
