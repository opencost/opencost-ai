# opencost-ai

> **Status: v0.1.0.** First tagged release. Gateway binary, Helm
> chart, and air-gap install flow ship as signed artefacts (cosign +
> SPDX SBOM + SLSA v1.0 provenance). See `CHANGELOG.md` for the v0.1
> contract and `docs/architecture.md` §11 for the shipped-vs-spec
> delta. The prototype in `legacy/prototype-flask/` is frozen and
> must not be built.

A Kubernetes-native, air-gap-deployable, open-source AI assistant for
[OpenCost](https://www.opencost.io/). It lets platform and FinOps
teams ask cost questions in natural language without sending cluster
data to a third-party LLM provider.

## What this is

A small Go HTTP gateway (`cmd/gateway`) in front of
[`jonigl/ollama-mcp-bridge`](https://github.com/jonigl/ollama-mcp-bridge),
which brokers a local [Ollama](https://ollama.com/) runtime and the
[OpenCost MCP server](https://www.opencost.io/) (built-in as of
OpenCost v1.118).

```
client  →  opencost-ai-gateway  →  ollama-mcp-bridge  →  Ollama
                                                       →  OpenCost MCP :8081
```

The gateway adds authentication, audit, rate limiting, prompt
guardrails, and a stable `/v1/*` HTTP contract around the
intentionally-unauthenticated Ollama `/api/chat` surface exposed by
the bridge.

## What this is not

- Not a general chatbot. It answers OpenCost-derived cost questions.
- Not a cost-recommendation engine. v0.1 exposes existing data through
  natural language; it does not prescribe.
- Not hosted. No SaaS in the open-source project.
- Not multi-cluster or federated. One OpenCost instance per
  deployment.

## Documentation

| Doc                          | Purpose                                                              |
|------------------------------|----------------------------------------------------------------------|
| `docs/architecture.md`       | Intent, target architecture, resolved decisions. §11 is the delta between the spec and what actually shipped in v0.1. |
| `docs/api.md`                | Operator-facing HTTP reference for every `/v1` route.                |
| `docs/prompts.md`            | The intended system prompt and its rationale.                        |
| `docs/security.md`           | STRIDE threat model and operator audit checklist.                    |
| `docs/air-gap-install.md`    | End-to-end offline install flow.                                     |
| `CHANGELOG.md`               | Release-by-release changes.                                          |
| `SECURITY.md`                | Vulnerability reporting policy.                                      |

If the code diverges from `docs/architecture.md`, the code is wrong;
if the intent is wrong, update the design doc in the same PR.

## Repository layout

```
opencost-ai/
├── CLAUDE.md                  # instructions for Claude Code sessions
├── CHANGELOG.md
├── LICENSE                    # Apache-2.0
├── README.md
├── cmd/gateway/               # main.go — wire-up only
├── internal/                  # server, auth, bridge, audit, ratelimit,
│                              # config, metrics, requestid
├── pkg/apiv1/                 # exported wire types, no behaviour
├── deploy/helm/opencost-ai/   # Helm chart: gateway + bridge + ollama
├── scripts/air-gap/           # ORAS export/push/pull, crane mirror
├── test/integration/          # gateway integration test
├── test/airgap/               # iptables egress-block e2e harness
├── docs/                      # architecture, api, prompts, security, air-gap
└── legacy/                    # archived Flask + pexpect prototype (do not build)
```

## Installation

Air-gap clusters: `docs/air-gap-install.md`. Connected clusters:

```sh
kubectl create namespace opencost-ai
kubectl label namespace opencost-ai \
  pod-security.kubernetes.io/enforce=restricted

kubectl -n opencost-ai create secret generic opencost-ai-auth \
  --from-literal=token="$(openssl rand -hex 32)"

helm install opencost-ai ./deploy/helm/opencost-ai \
  --namespace opencost-ai \
  --set gateway.auth.existingSecret=opencost-ai-auth
```

Verify the image signature and SBOM before deploying — see
`docs/security.md` §6.

## License

Apache-2.0. See `LICENSE`.

## Contributing

See `CONTRIBUTING.md` for DCO sign-off, GPG-signed commits, branch and
commit conventions, and the security checklist. Security issues go
through `SECURITY.md`, not the public issue tracker.
