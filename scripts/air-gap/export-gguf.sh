#!/usr/bin/env bash
# Resolve an Ollama model tag to the GGUF blob that backs it.
#
# Usage:
#   export-gguf.sh <model-tag> <output-path>
#
# Example:
#   export-gguf.sh qwen2.5:7b-instruct ./stage/qwen2.5-7b-instruct.gguf
#
# The Ollama on-disk layout stores each model as a manifest plus a set
# of sha256-addressed blobs. The manifest references one layer with
# mediaType application/vnd.ollama.image.model — that layer is the
# GGUF. This script reads the manifest, locates the blob, copies it to
# the requested path, and also emits the Modelfile beside it so the
# air-gap side has everything needed for `ollama create`.
#
# Dependencies: ollama, jq, sha256sum.

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <model-tag> <output-path>" >&2
  exit 2
fi

tag="$1"
out="$2"

for cmd in ollama jq sha256sum; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}" >&2
    exit 3
  fi
done

# Confirm the tag is present locally; run `ollama pull` separately on a
# connected host if it is not. This script deliberately does not call
# `ollama pull` on the caller's behalf — the whole point is that the
# next step may run disconnected.
if ! ollama list 2>/dev/null | awk 'NR>1 {print $1}' | grep -qx "${tag}"; then
  echo "model tag not found locally: ${tag}" >&2
  echo "run: ollama pull ${tag}" >&2
  exit 4
fi

ollama_home="${OLLAMA_MODELS:-${HOME}/.ollama/models}"
if [[ ! -d "${ollama_home}" ]]; then
  echo "ollama model store not found at ${ollama_home}" >&2
  echo "set OLLAMA_MODELS if you overrode the default" >&2
  exit 5
fi

# Manifest path convention: manifests/<registry>/<ns>/<name>/<tag>.
# For `ollama pull` of library models that's registry.ollama.ai/library;
# other pull sources (Ollama Registry, private mirrors) land under
# their own registry host directory. Try the canonical path first,
# then fall back to a glob over the manifests tree.
name="${tag%%:*}"
ver="${tag#*:}"
manifest_path=""
canonical="${ollama_home}/manifests/registry.ollama.ai/library/${name}/${ver}"
if [[ -f "${canonical}" ]]; then
  manifest_path="${canonical}"
fi

if [[ -z "${manifest_path}" ]]; then
  # Users pulling from alternate registries end up under a different
  # path prefix. The (name, tag) pair alone is unique enough under
  # manifests/ to disambiguate in practice.
  manifest_path="$(find "${ollama_home}/manifests" -type f -path "*/${name}/${ver}" -print -quit 2>/dev/null || true)"
fi

if [[ -z "${manifest_path}" || ! -f "${manifest_path}" ]]; then
  echo "could not locate manifest for ${tag} under ${ollama_home}/manifests" >&2
  exit 6
fi

# Each layer object has digest (sha256:...) and mediaType. The GGUF
# layer is application/vnd.ollama.image.model.
gguf_digest="$(jq -r '
  .layers[]
  | select(.mediaType == "application/vnd.ollama.image.model")
  | .digest
' "${manifest_path}")"

modelfile_digest="$(jq -r '
  .layers[]
  | select(.mediaType == "application/vnd.ollama.image.modelfile")
  | .digest
' "${manifest_path}")"

if [[ -z "${gguf_digest}" || "${gguf_digest}" == "null" ]]; then
  echo "manifest for ${tag} has no application/vnd.ollama.image.model layer" >&2
  exit 7
fi

# Blob path: blobs/sha256-<hex>  (Ollama replaces the ':' with '-').
blob_file="${ollama_home}/blobs/${gguf_digest/:/-}"
if [[ ! -f "${blob_file}" ]]; then
  echo "GGUF blob missing on disk: ${blob_file}" >&2
  exit 8
fi

mkdir -p "$(dirname "${out}")"
cp -f "${blob_file}" "${out}"

# Verify the checksum end-to-end. A silent corruption here would be
# nearly impossible to diagnose on the cluster side, so we pay the
# extra sha256sum pass on the staging host where debugging is cheap.
expected="${gguf_digest#sha256:}"
actual="$(sha256sum "${out}" | awk '{print $1}')"
if [[ "${expected}" != "${actual}" ]]; then
  echo "checksum mismatch: expected ${expected}, got ${actual}" >&2
  rm -f "${out}"
  exit 9
fi

# Emit the Modelfile beside the GGUF when present. Not every Ollama
# tag ships with a separate Modelfile layer — some embed system
# prompts in the manifest's config. Handle both.
if [[ -n "${modelfile_digest}" && "${modelfile_digest}" != "null" ]]; then
  modelfile_blob="${ollama_home}/blobs/${modelfile_digest/:/-}"
  if [[ -f "${modelfile_blob}" ]]; then
    cp -f "${modelfile_blob}" "${out%.gguf}.Modelfile"
  fi
fi

# If there is no explicit Modelfile blob, synthesise a minimal one
# pointing at the GGUF filename. `ollama create` accepts this form.
if [[ ! -f "${out%.gguf}.Modelfile" ]]; then
  cat > "${out%.gguf}.Modelfile" <<MODELFILE
# Synthesised by export-gguf.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)
# for tag: ${tag}
FROM ./$(basename "${out}")
MODELFILE
fi

echo "wrote GGUF:      ${out}"
echo "wrote Modelfile: ${out%.gguf}.Modelfile"
echo "sha256:          ${actual}"
