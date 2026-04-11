[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

# opencost-ai

Natural language interface for querying OpenCost cost data using local LLMs.

> **Status: Early-stage / experimental.**
> This project is a proof-of-concept. APIs and architecture may change significantly.

---

## How It Works

opencost-ai runs an HTTP API server that accepts natural language questions
about Kubernetes costs and returns answers by connecting a local Ollama LLM
to an OpenCost MCP server.
User
│  POST /query {"query": "show me allocation costs for last 2 days"}
▼
HTTP API Server (Flask, port 8888)
│
│  pexpect subprocess
▼
ollmcp CLI
│
│  Ollama API (port 11434)
▼
Local LLM (default: qwen2.5:0.5b)
│
│  MCP streamable_http
▼
OpenCost MCP Server (port 8081)
│
▼
OpenCost cost data

All components except OpenCost itself run inside a single Docker container.

---

## Prerequisites

- **Docker** — to build and run the container
- **OpenCost** deployed with the MCP server enabled.
  The MCP server is enabled by default in all OpenCost deployments since v1.118.
  See: [OpenCost MCP Server docs](https://www.opencost.io/docs/integrations/mcp/)
- **Kubernetes** cluster (OpenCost requires one to collect cost data)
- Ollama is included in the Docker image — no separate installation needed

---

## Quick Start

```bash
# Build from repository root
docker build -f docker/Dockerfile.ollama -t opencost-ai .

# Run — set MCP_SERVER_URL to your OpenCost instance
docker run -p 8888:8888 \
  -e MCP_SERVER_URL=http://<your-opencost-host>:8081/mcp \
  opencost-ai
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_SERVER_URL` | `http://opencost.default.svc.cluster.local:8081/mcp` | OpenCost MCP server endpoint. Default assumes Kubernetes with OpenCost in the `default` namespace. For local development use `http://localhost:8081/mcp`. |
| `DEFAULT_MODEL` | `qwen2.5:0.5b` | Ollama model to use for queries. Must be available inside the container. |

---

## API Reference

### GET /health

```bash
curl http://localhost:8888/health
```

```json
{"status": "healthy", "default_model": "qwen2.5:0.5b"}
```

### POST /query

```bash
curl -X POST http://localhost:8888/query \
  -H "Content-Type: application/json" \
  -d '{"query": "show me allocation costs for the last 2 days"}'
```

```json
{"success": true, "query": "...", "result": "...", "model": "qwen2.5:0.5b"}
```

Optional: pass `"model"` in the request body to use a different Ollama model.

### GET /models

```bash
curl http://localhost:8888/models
```

```json
{"models": [{"name": "qwen2.5:0.5b", "size": "397 MB"}], "default_model": "qwen2.5:0.5b"}
```

### GET /tools

```bash
curl http://localhost:8888/tools
```

```json
{"tools": [{"name": "opencost.getallocationdata", "description": ""}], "mcp_server": "opencost", "count": 1}
```

> **Note:** Tool descriptions are currently empty. This is a known limitation.

---

## Roadmap

| Phase | Status | Description |
|-------|--------|-------------|
| Phase 1: Foundation | 🔄 In progress | README, CI/CD, pinned dependencies, GitHub templates |
| Phase 2: Code quality | 🔲 Planned | Input validation, type annotations, unit tests, Dockerfile fixes |
| Phase 3: Direct MCP SDK integration | 🔲 Planned | Replace pexpect/ollmcp CLI with [MCP Python SDK](https://github.com/modelcontextprotocol/python-sdk) direct calls |
| Phase 4: Model benchmarking | 🔲 Planned | Evaluate models at scale, document smallest viable model for production |

---

## Contributing

Please open an issue before starting significant work.

Join the community on [CNCF Slack](https://slack.cncf.io/) in the **#opencost** channel.

---

## License

Apache 2.0 — see [LICENSE](LICENSE)
