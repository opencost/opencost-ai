# test/airgap

End-to-end validation that the `opencost-ai` chart installs and serves
traffic inside a cluster with **no public-internet egress**. This is
the feature that justifies the whole project, and render-only CI
cannot prove it — this harness must fail when the air-gap posture
degrades, even if the happy path otherwise works.

## How egress is blocked

`run.sh` installs a host-level `iptables` rule in `DOCKER-USER` that
drops packets originating from the kind bridge, with a RETURN rule in
front of it for traffic destined to the kind network CIDR so the
in-cluster registry stays reachable:

```
-I DOCKER-USER 1 -i <kind-bridge> -d <kind-cidr> -j RETURN   (tagged "opencost-ai-airgap-e2e")
-I DOCKER-USER 1 -i <kind-bridge>                -j DROP     (tagged "opencost-ai-airgap-e2e")
```

The rule is scoped by source interface so only pod-originated egress
is affected — the runner's own traffic (fetching `kind`, `helm`,
`crane` earlier in the job) is unaffected.

Cleanup is trapped on `EXIT` and matches by comment tag, so a failed
run cannot leave stray drops on the host.

Requires `sudo` because it touches `iptables`. A previous revision
exposed a `--mode=netns` no-sudo alternative that leaned on Docker
network isolation; it was removed after review because kind's default
Docker bridge has public egress and a netns-only block would pass
the egress assertion only when the probe pod could not resolve DNS,
not because the perimeter was enforced. Theatrical security is worse
than no security.

## What the harness asserts

`run.sh` fails the build if any of these are false:

1. `helm install` with `gateway.image.repository` pointing at the
   in-cluster registry succeeds and the pod reaches Ready.
2. `/v1/health` returns `{"status":"ok", ...}` from a pod inside
   the chart's namespace.
3. A control-case pod in the same namespace **cannot** reach
   `https://1.1.1.1` within 5 seconds — confirms the block is real.
4. A control-case pod can still reach the in-cluster registry by DNS
   — confirms the block is scoped, not global.
5. The Deployment's image reference resolves to
   `opencost-ai-registry:5000/…`, not `ghcr.io/…` — catches a silent
   fallback where the egress block was misapplied.

Assertion 3 is the load-bearing one: without it, the test is
theatrical. Assertion 5 is a backstop against a future regression
where the chart template drops the override.

## What the harness does not do

- **It does not stand up a real Ollama + bridge.** The bridge's
  startup probe needs a responding MCP backend; shipping either a
  real OpenCost or a faithful MCP stub inflates the CI budget past
  what PR gating can absorb. The gateway-only profile here matches
  `deploy/helm/opencost-ai/ci/integration-values.yaml` in shape so a
  template-rendering regression surfaces in both jobs. Full-stack
  validation lives in a nightly job on a larger runner.
- **It does not run a real 7B model.** A `qwen2.5:7b-instruct`
  container image + weights is ~5 GB. The harness does exercise the
  ORAS push path with a synthetic multi-KB GGUF when `oras` is on
  PATH — enough to catch a regression in the push/pull scripts
  without paying the model-weight cost.
- **It does not test OpenCost MCP integration.** That is covered by
  `test/integration/` against the real bridge with the MCP layer
  stubbed.
- **It does not prove model licence compliance.** Mirroring real
  weights remains the operator's responsibility.

## Running locally

```sh
# Default — requires sudo for the iptables step:
sudo test/airgap/run.sh

# Also mirror real ollama + bridge images into the in-cluster registry
# (proves the mirror path without deploying them):
sudo test/airgap/run.sh --mirror-upstream

# Keep the cluster up for poking around after the assertions run:
sudo test/airgap/run.sh --keep
```

With `--keep`, the harness leaves the kind cluster running so you
can inspect it manually with `kubectl` after the assertions complete
(the kind context is `kind-${CLUSTER_NAME}` — by default
`kind-opencost-ai-airgap`).

## Layout

```
test/airgap/
├── README.md             # this file
├── run.sh                # harness entrypoint
├── kind-config.yaml      # kind cluster config (1 control-plane node)
└── values-airgap.yaml    # Helm values overriding image.repository
```
