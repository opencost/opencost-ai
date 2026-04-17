#!/usr/bin/env bash
# Pull an OCI model artefact and register it with Ollama.
#
# Usage:
#   oras-pull-model.sh <oci-ref> <model-tag> [<workdir>]
#
# Example:
#   oras-pull-model.sh \
#     registry.internal.example/ollama-model/qwen2.5-7b-instruct:latest \
#     qwen2.5:7b-instruct
#
# Intended to run:
#   - Inside the in-cluster model-bootstrap Job (see
#     docs/air-gap-install.md §3a), OR
#   - On a helper pod with the Ollama PVC mounted at $HOME.
#
# The script is idempotent: if the tag is already registered with a
# matching blob, `ollama create` is a no-op on the PVC.
#
# Dependencies: oras, ollama.

set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "usage: $0 <oci-ref> <model-tag> [<workdir>]" >&2
  exit 2
fi

ref="$1"
tag="$2"
workdir="${3:-$(mktemp -d)}"

for cmd in oras ollama; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}" >&2
    exit 3
  fi
done

oras_flags=()
if [[ "${ORAS_PLAIN_HTTP:-0}" == "1" ]]; then
  oras_flags+=("--plain-http")
fi

mkdir -p "${workdir}"
# `oras pull` deposits each layer at the filename recorded in the
# manifest. The push side (scripts/air-gap/oras-push-model.sh)
# enforces a `.gguf` / `.Modelfile` extension convention, so we can
# locate the blobs by extension here without re-parsing the
# manifest. Artefacts pushed by other tooling that drop the
# convention will not load — flag and exit rather than silently
# pick the wrong file.
oras pull "${oras_flags[@]}" --output "${workdir}" "${ref}"

gguf="$(find "${workdir}" -maxdepth 1 -name '*.gguf' -print -quit)"
modelfile="$(find "${workdir}" -maxdepth 1 -name '*.Modelfile' -print -quit)"

if [[ -z "${gguf}" ]]; then
  echo "no *.gguf found in ${workdir} after oras pull" >&2
  echo "the artefact at ${ref} must carry a layer with a .gguf filename" >&2
  echo "(push artefacts using scripts/air-gap/oras-push-model.sh to enforce this)" >&2
  exit 4
fi

if [[ -z "${modelfile}" ]]; then
  # Synthesise a minimal Modelfile. `ollama create` requires one.
  modelfile="${workdir}/synth.Modelfile"
  cat > "${modelfile}" <<MODELFILE
# Synthesised by oras-pull-model.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# for tag: ${tag}
FROM ./$(basename "${gguf}")
MODELFILE
fi

# `ollama create` resolves FROM relative to the Modelfile's directory.
(
  cd "$(dirname "${modelfile}")"
  ollama create "${tag}" -f "$(basename "${modelfile}")"
)

echo "registered ${tag} from ${ref}"
ollama list | grep -E "^${tag//\//\\/}\\b" || true
