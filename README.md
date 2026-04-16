# opencost-ai

> **Status: pre-v0.1, not yet usable.**
> No tagged release, no Helm chart, no runnable gateway binary. The
> prototype in `legacy/prototype-flask/` is frozen and must not be
> built. Do not deploy this repository into anything you care about.

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

## Architecture and scope

`docs/architecture.md` is the source of truth for intent, scope, and
resolved decisions. Read it before making any non-trivial change.
Relevant sections:

- §2 — teardown of the archived prototype
- §5 — product thesis and non-goals for v0.1
- §6 — target architecture
- §7 — `opencost-ai-gateway` specification
- §10 — resolved architectural decisions (binding)

If the code diverges from `docs/architecture.md`, the code is wrong;
if the intent is wrong, update the design doc in the same PR.

## Repository layout (current)

```
opencost-ai/
├── CLAUDE.md                  # instructions for Claude Code sessions
├── LICENSE                    # Apache-2.0
├── README.md                  # this file
├── docs/
│   └── architecture.md        # design of record
└── legacy/
    ├── README.md              # why the prototype is frozen
    └── prototype-flask/       # archived Flask + pexpect prototype
        ├── docker/
        │   └── Dockerfile.ollama
        └── src/
            └── ollmcp-api-server.py
```

The `cmd/`, `internal/`, `pkg/`, `deploy/`, and `test/` trees land in
subsequent commits as the v0.1 scaffold fills in.

## License

Apache-2.0. See `LICENSE`.

## Contributing

See `CONTRIBUTING.md` for DCO sign-off, GPG-signed commits, branch and
commit conventions, and the security checklist. Security issues go
through `SECURITY.md`, not the public issue tracker.
