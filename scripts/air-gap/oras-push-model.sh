#!/usr/bin/env bash
# Push a GGUF + Modelfile pair to an OCI registry as a single artefact.
#
# Usage:
#   oras-push-model.sh <oci-ref> <gguf-path>
#
# Example:
#   oras-push-model.sh \
#     registry.internal.example/ollama-model/qwen2.5-7b-instruct:latest \
#     ./stage/qwen2.5-7b-instruct.gguf
#
# Artefact layout:
#   manifest (application/vnd.oci.image.manifest.v1+json)
#     config:  application/vnd.ollama.image.config+json
#     layer 0: application/vnd.ollama.image.model       <GGUF>
#     layer 1: application/vnd.ollama.image.modelfile   <Modelfile>
#
# The media types are deliberately the same Ollama uses natively, so
# the artefact round-trips through Ollama tooling on the cluster side
# and an air-gap registry admin can apply policies per mediaType.
#
# Dependencies: oras.

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <oci-ref> <gguf-path>" >&2
  exit 2
fi

ref="$1"
gguf="$2"
modelfile="${gguf%.gguf}.Modelfile"

if ! command -v oras >/dev/null 2>&1; then
  echo "missing required command: oras (https://oras.land)" >&2
  exit 3
fi

if [[ ! -f "${gguf}" ]]; then
  echo "GGUF not found: ${gguf}" >&2
  exit 4
fi

if [[ ! -f "${modelfile}" ]]; then
  echo "Modelfile not found: ${modelfile}" >&2
  echo "run scripts/air-gap/export-gguf.sh first to produce it" >&2
  exit 5
fi

# ORAS push supports per-file media types via the `:<type>` suffix. The
# artefact type (config media type) is set via --artifact-type; the
# layer media types are set per-file.
#
# Working directory: ORAS records file names as they are on disk. We
# `cd` into the containing directory so the manifest records just the
# basenames, not the staging path, which keeps pulls clean on the
# cluster side.
stage_dir="$(cd "$(dirname "${gguf}")" && pwd)"
gguf_base="$(basename "${gguf}")"
modelfile_base="$(basename "${modelfile}")"

oras_flags=()
if [[ "${ORAS_PLAIN_HTTP:-0}" == "1" ]]; then
  # Plain-HTTP registries (in-cluster test rigs, Zot with TLS off)
  # need explicit opt-in. Production registries run TLS; default-off
  # keeps the opt-in visible in reviews.
  oras_flags+=("--plain-http")
fi

(
  cd "${stage_dir}"
  oras push \
    "${oras_flags[@]}" \
    --artifact-type application/vnd.ollama.image.config+json \
    "${ref}" \
    "${gguf_base}:application/vnd.ollama.image.model" \
    "${modelfile_base}:application/vnd.ollama.image.modelfile"
)

# Emit the digest in a machine-parseable form so the caller can
# feed it into values.yaml or a GitOps commit without re-parsing.
# `oras manifest fetch --descriptor` returns JSON; prefer jq when
# available over the grep fallback, so a future oras output format
# change can't slip a wrong value through.
descriptor="$(oras manifest fetch "${oras_flags[@]}" --descriptor "${ref}" 2>/dev/null || true)"
if command -v jq >/dev/null 2>&1 && [[ -n "${descriptor}" ]]; then
  digest="$(printf '%s' "${descriptor}" | jq -r '.digest // empty')"
else
  digest="$(printf '%s' "${descriptor}" | grep -oE 'sha256:[0-9a-f]{64}' | head -n1 || true)"
fi
if [[ -n "${digest}" ]]; then
  echo "pushed ${ref}"
  echo "digest ${digest}"
else
  echo "pushed ${ref}"
fi
