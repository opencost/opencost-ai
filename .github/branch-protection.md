# Branch protection rules

Branch protection rules are configured in GitHub's repository settings,
not the repo. This file documents the exact rules a maintainer must
apply so the configuration is reproducible and reviewable.

Apply these rules after the default branch has been renamed to
`develop` (see PR description for the full post-merge checklist).

Navigate to:

> Settings → Branches → Branch protection rules → Add branch
> protection rule

## `develop`

- **Branch name pattern:** `develop`
- **Require a pull request before merging:** ON
  - **Require approvals:** 1
  - **Dismiss stale pull request approvals when new commits are
    pushed:** ON
  - **Require review from Code Owners:** ON (see `CODEOWNERS`)
  - **Require approval of the most recent reviewable push:** ON
  - **Restrict who can dismiss pull request reviews:** unrestricted
  - The reviewer must be someone other than the PR author; this is
    enforced by "Require approvals: 1" combined with GitHub's
    built-in self-approval prohibition.
- **Require status checks to pass before merging:** ON
  - **Require branches to be up to date before merging:** ON
  - **Required status checks:**
    - `ci / go vet`
    - `ci / go test`
    - `ci / staticcheck`
    - `ci / govulncheck`
    - `ci / build distroless image (no push)`
    - `helm / helm lint + template`
    - `helm / kind + helm install + /v1/health`
    - `airgap-e2e / kind + iptables egress block + gateway-only install`
    - `authorship / reject assistant authorship + DCO sign-off`
    - `DCO` — provided by the [DCO GitHub App](https://github.com/apps/dco);
      install is a post-merge step.
    - `Trivy Vulnerability Scanner / Scan for Vulnerabilities`
    - `Scorecard supply-chain security / Scorecard analysis`
    - `CodeQL Advanced / Analyze (go)`
    - `CodeQL Advanced / Analyze (actions)`
    - `CodeQL Advanced / Analyze (python)`
- **Require signed commits:** OFF
  - DCO sign-off is the gate. GPG signing is not required; this
    matches upstream opencost.
- **Require linear history:** ON
  - Keeps `git log --oneline` auditable for the authorship hygiene
    gate.
- **Require conversation resolution before merging:** ON
- **Require deployments to succeed before merging:** OFF
- **Lock branch:** OFF
- **Do not allow bypassing the above settings:** ON
- **Restrict who can push to matching branches:** OFF (pushes to
  `develop` happen only via merged PRs)
- **Rules applied to everyone including administrators:** ON
- **Allow force pushes:** OFF
- **Allow deletions:** OFF

## `main`

`main` is reserved for released-version pointers. It advances only
via fast-forward merge from a release branch after a successful
release.

- **Branch name pattern:** `main`
- Same settings as `develop`, with these additions and differences:
  - **Restrict who can push to matching branches:** ON. Allowed
    pushers: `@opencost/opencost-ai-maintainers` team only.
  - **Require approvals:** 1 (maintainer review is already implicit
    by the restriction above; the approval count keeps the four-eyes
    property for fast-forward PRs).
  - **Required status checks:** same list as `develop`, plus any
    release-specific checks that become available.

## `v*` (release branches, e.g. `v0.1`, `v0.2`)

Pattern matches every release branch cut from `develop` when a minor
is ready to release.

- **Branch name pattern:** `v*`
  - GitHub branch-protection patterns use `fnmatch`-style globbing;
    `v*` matches `v0.1`, `v0.2`, `v1.0`, etc. It does NOT match tags
    (`v0.1.0`): tag protection is configured separately under the
    **Tag protection rules** section.
- Same settings as `main`:
  - **Restrict who can push to matching branches:** ON. Allowed
    pushers: `@opencost/opencost-ai-maintainers` team only.
  - Same required status checks as `develop`.
  - **Require linear history:** ON. Patches for a released minor
    land on the release branch via PR, then cherry-pick forward to
    `develop` if relevant.
  - **Allow force pushes:** OFF.
  - **Allow deletions:** OFF (release branches are preserved for
    auditability).

## Tag protection

Navigate to:

> Settings → Tags → New rule

- **Tag name pattern:** `v*.*.*`
- **Restrict who can create matching tags:** ON. Allowed creators:
  `@opencost/opencost-ai-maintainers` team only.

Tag creation triggers `.github/workflows/release.yml`, which in turn
derives `v<MAJOR>.<MINOR>` from the tag and sources the release
branch. Restricting tag creation keeps the release path to
maintainers only.

## Verification

After applying the rules:

1. Attempt to push directly to `develop` as a non-maintainer — must
   be rejected.
2. Open a PR to `develop`, leave it unapproved — the "Merge" button
   must be disabled until the status checks pass and a non-author
   approval is recorded.
3. Attempt to force-push to `develop` or a `v*` branch — must be
   rejected.
4. Attempt to create a `v0.0.1-test` tag as a non-maintainer — must
   be rejected.

If any of these pass when they should fail, a rule is missing or
mis-scoped. Re-check the settings against this document.
