# legacy/

Frozen historical code. Not built, not tested, not shipped.

## `prototype-flask/`

The original `opencost-ai` prototype: a ~210-line Flask script
(`src/ollmcp-api-server.py`) that shells out to the `ollmcp` TUI with
`pexpect` and regex-parses its ANSI-stripped output, plus a ~160-line
`docker/Dockerfile.ollama` that bakes Ollama, `ollmcp`, Flask, and a
0.5B model into a single root-owned image.

It is preserved here — not deleted — for three reasons:

1. **Archaeology.** Anyone coming to the repo cold can see what was
   tried and why it was abandoned, without digging through git history.
2. **Contract reference.** The `/query` request/response shape is the
   only part of the prototype worth carrying forward; the new
   `POST /v1/ask` handler cribs its field names from there.
3. **Honesty.** Deleting the prototype would hide the starting point.

## Why it was replaced

See `docs/architecture.md` §2 for the full teardown. In short:

- Driving an interactive TUI with `pexpect` and scraping regex groups
  out of ANSI-stripped output is not an integration contract. Any
  version bump of `ollmcp` breaks it silently.
- `ollama-mcp-bridge` (same author as `ollmcp`) exposes the same
  capability over a native Ollama `/api/chat` HTTP API. Consuming that
  directly removes the TUI-scraping layer entirely.
- The container ran as root, had no auth, bound `0.0.0.0`, spawned a
  subprocess per request, and returned raw exception strings to
  callers. None of that is fixable incrementally.

## Do not

- Do not build this image.
- Do not extend this code.
- Do not import from this directory.
- Do not reintroduce the TUI-scraping approach in new code
  (CLAUDE.md "Non-negotiables").

New development lives under `cmd/`, `internal/`, `pkg/`, and
`deploy/` per the target layout in `docs/architecture.md` §7.8.
