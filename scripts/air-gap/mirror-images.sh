#!/usr/bin/env bash
# Copy a list of container images from their public source to an
# internal registry while preserving manifest digests.
#
# Usage:
#   mirror-images.sh <src1=dst1> [<src2=dst2> ...]
#
# Example:
#   mirror-images.sh \
#     ghcr.io/opencost/opencost-ai-gateway:v0.1.0=registry.internal.example/opencost-ai/opencost-ai-gateway:v0.1.0 \
#     ghcr.io/jonigl/ollama-mcp-bridge:v0.2.0=registry.internal.example/opencost-ai/ollama-mcp-bridge:v0.2.0 \
#     ollama/ollama:0.6.0=registry.internal.example/opencost-ai/ollama:0.6.0
#
# For doubly-isolated environments (staging host cannot reach the
# internal registry), set OCI_LAYOUT=/path/to/layout to drop the
# pulled images into an OCI layout directory. Later, re-run with
# OCI_LAYOUT=/path/to/layout in push-only mode from inside the
# perimeter.
#
# Dependencies: crane (go-containerregistry).

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <src=dst> [<src=dst> ...]" >&2
  exit 2
fi

if ! command -v crane >/dev/null 2>&1; then
  echo "missing required command: crane (go-containerregistry)" >&2
  exit 3
fi

layout="${OCI_LAYOUT:-}"

# Plain-HTTP registries (in-cluster test rigs, Zot with TLS off) need
# --insecure on every crane verb that touches the destination. Build
# the flag once so both code paths (direct copy and OCI_LAYOUT push)
# stay consistent.
crane_insecure=()
if [[ "${CRANE_INSECURE:-0}" == "1" ]]; then
  crane_insecure+=("--insecure")
fi

for pair in "$@"; do
  if [[ "${pair}" != *=* ]]; then
    echo "malformed argument (expected src=dst): ${pair}" >&2
    exit 4
  fi
  src="${pair%%=*}"
  dst="${pair#*=}"

  if [[ -n "${layout}" ]]; then
    # Two-step (pull to layout, push from layout). Operators run this
    # script twice: once on the connected side with OCI_LAYOUT pointed
    # at a sneakernet target, and once on the disconnected side with
    # OCI_LAYOUT pointing at the same layout after transport.
    if crane manifest "${crane_insecure[@]}" "${dst}" >/dev/null 2>&1; then
      echo "already present at ${dst}; skipping"
      continue
    fi

    mkdir -p "${layout}"
    # Pull side: emit a flat OCI layout. Push side: read from it.
    if [[ -z "${AIRGAP_LAYOUT_MODE:-}" ]]; then
      echo "AIRGAP_LAYOUT_MODE must be 'pull' or 'push' when OCI_LAYOUT is set" >&2
      exit 5
    fi
    case "${AIRGAP_LAYOUT_MODE}" in
      pull)
        # Source side is the public registry, which is HTTPS — do
        # not propagate --insecure here even if CRANE_INSECURE=1
        # was set for the destination.
        crane pull --format oci "${src}" "${layout}/${src//\//_}.oci"
        echo "pulled ${src} -> ${layout}/${src//\//_}.oci"
        ;;
      push)
        crane push "${crane_insecure[@]}" "${layout}/${src//\//_}.oci" "${dst}"
        digest="$(crane digest "${crane_insecure[@]}" "${dst}")"
        printf 'pushed %s (%s)\n' "${dst}" "${digest}"
        ;;
      *)
        echo "unknown AIRGAP_LAYOUT_MODE: ${AIRGAP_LAYOUT_MODE}" >&2
        exit 6
        ;;
    esac
  else
    # Direct copy. `crane copy` preserves the manifest byte-for-byte
    # so the destination digest equals the source digest when the
    # registry supports it.
    crane copy "${crane_insecure[@]}" "${src}" "${dst}"
    digest="$(crane digest "${crane_insecure[@]}" "${dst}")"
    printf 'copied %s -> %s (%s)\n' "${src}" "${dst}" "${digest}"
  fi
done
