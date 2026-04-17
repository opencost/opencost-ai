#!/usr/bin/env bash
# Air-gap install end-to-end harness.
#
# This script is the validation for the feature that justifies the
# whole project. It is deliberately not a render-only check: it boots
# kind, blocks pod egress at the host firewall via iptables, mirrors
# the gateway image into an in-cluster registry, installs the chart
# pointed only at that registry, and asserts that:
#
#   (a) the chart installs and the gateway pod reaches Ready using
#       an image pulled from the in-cluster registry, AND
#   (b) pods in the chart's namespace cannot reach the public
#       internet — the egress block is real, not theatrical, AND
#   (c) pods can still reach the in-cluster registry — the block
#       is scoped rather than global.
#
# If (b) fails, the test fails the build even when (a) succeeds.
# That is the point.
#
# Scope: this harness exercises the install wiring (mirrored images,
# overridden image.repository values, NetworkPolicy egress posture,
# auth Secret, ORAS model-push path). It does not stand up a real
# Ollama + bridge + MCP server — the bridge's startup probe needs a
# responding MCP backend, and shipping either a real OpenCost or a
# faithful MCP stub inflates the CI budget past what PR gating can
# absorb. Full-stack validation lives in a nightly job (see
# .github/workflows/airgap-e2e.yml), and the chart's gateway-only
# install shape is the load-bearing assertion here.
#
# Usage:
#   sudo test/airgap/run.sh                 # CI default
#   sudo test/airgap/run.sh --keep          # keep kind + rules after the run
#   sudo test/airgap/run.sh --mirror-upstream    # also `crane copy` real
#                                           # ollama + bridge images into
#                                           # the registry (proves the
#                                           # mirror path for a full
#                                           # install; does not deploy them).
#
# Root is required: the egress block is an iptables DOCKER-USER rule.
# A sandbox-only "netns" mode was considered and removed — kind's
# default docker bridge has public egress, so a netns-only block
# would silently pass assertion 2 ("curl 1.1.1.1 must fail") only
# because the probe pod could not resolve DNS, not because the
# perimeter was enforced. Better to have no mode than a theatrical
# one.
#
# Dependencies:
#   kind, kubectl, helm, docker, crane, iptables
#   oras (optional; skipped with a warning when missing)

set -euo pipefail

# --- flags --------------------------------------------------------------

KEEP=0
MIRROR_UPSTREAM=0
CLUSTER_NAME="opencost-ai-airgap"
NAMESPACE="opencost-ai"
REGISTRY_NAME="opencost-ai-registry"
REGISTRY_PORT="5000"
IPT_COMMENT="opencost-ai-airgap-e2e"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep)           KEEP=1; shift ;;
    --mirror-upstream) MIRROR_UPSTREAM=1; shift ;;
    --cluster-name=*) CLUSTER_NAME="${1#--cluster-name=}"; shift ;;
    -h|--help)
      sed -n '2,45p' "$0"
      exit 0
      ;;
    *)
      echo "unknown flag: $1" >&2
      exit 2
      ;;
  esac
done

# --- prechecks ----------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

for cmd in kind kubectl helm docker crane iptables; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}" >&2
    exit 3
  fi
done

if [[ "$(id -u)" -ne 0 ]]; then
  echo "this harness needs root to install the DOCKER-USER iptables rule" >&2
  echo "re-run under sudo" >&2
  exit 4
fi

# --- teardown trap ------------------------------------------------------

KIND_BRIDGE=""

cleanup() {
  local rc=$?

  if [[ -n "${KIND_BRIDGE}" ]]; then
    # Remove any rule we inserted (match by comment tag so repeated
    # runs cannot accumulate rules or mistakenly delete unrelated ones).
    while iptables -C DOCKER-USER -i "${KIND_BRIDGE}" -m comment --comment "${IPT_COMMENT}" -j DROP 2>/dev/null; do
      iptables -D DOCKER-USER -i "${KIND_BRIDGE}" -m comment --comment "${IPT_COMMENT}" -j DROP || true
    done
    # Clean up the allow-to-registry RETURN rule as well.
    while iptables -S DOCKER-USER | grep -q -- "--comment ${IPT_COMMENT} -j RETURN"; do
      rule_spec="$(iptables -S DOCKER-USER | grep -- "--comment ${IPT_COMMENT} -j RETURN" | head -n1 | sed 's/^-A /-D /')"
      # shellcheck disable=SC2086
      iptables ${rule_spec} || break
    done
  fi

  if [[ "${KEEP}" -eq 0 ]]; then
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  else
    echo ""
    echo "--keep was set; resources left running:"
    echo "  kind cluster:   ${CLUSTER_NAME}"
    echo "  registry:       ${REGISTRY_NAME} (docker ps)"
    echo "  iptables rule:  DOCKER-USER -i ${KIND_BRIDGE} -j DROP (still in place)"
  fi

  exit "${rc}"
}
trap cleanup EXIT

# --- step 1: kind cluster + local registry ------------------------------

echo "==> creating kind cluster ${CLUSTER_NAME}"
kind create cluster \
  --name "${CLUSTER_NAME}" \
  --config "${REPO_ROOT}/test/airgap/kind-config.yaml" \
  --wait 120s

echo "==> starting in-cluster registry ${REGISTRY_NAME}:${REGISTRY_PORT}"
if [[ -z "$(docker ps -q -f name="^${REGISTRY_NAME}$")" ]]; then
  docker run -d --restart=always \
    --name "${REGISTRY_NAME}" \
    --network kind \
    --hostname "${REGISTRY_NAME}" \
    -p "127.0.0.1:${REGISTRY_PORT}:${REGISTRY_PORT}" \
    registry:2 >/dev/null
fi

# --- step 2: mirror images into the in-cluster registry -----------------

GATEWAY_IMG="${REGISTRY_NAME}:${REGISTRY_PORT}/opencost-ai-gateway:airgap-e2e"

echo "==> building gateway image for air-gap test"
docker build \
  --build-arg VERSION=airgap-e2e \
  --build-arg REVISION="$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || echo unknown)" \
  -t "opencost-ai-gateway:airgap-e2e" \
  -f "${REPO_ROOT}/Dockerfile" \
  "${REPO_ROOT}" >/dev/null

# Push the local build into the in-cluster registry. The registry
# container exposes :5000 on 127.0.0.1 on the host and is reachable
# as `opencost-ai-registry:5000` from inside the kind network (see
# kind-config.yaml's containerd mirror stanza). Pushing by the
# localhost-bound address keeps the daemon happy; pods pull by the
# kind-network hostname — both addresses resolve to the same
# registry container.
echo "==> pushing gateway image -> ${GATEWAY_IMG}"
LOCAL_PUSH_REF="localhost:${REGISTRY_PORT}/opencost-ai-gateway:airgap-e2e"
docker tag "opencost-ai-gateway:airgap-e2e" "${LOCAL_PUSH_REF}"
docker push "${LOCAL_PUSH_REF}"

# Optional: also mirror real upstream images. Proves the mirror path
# for bridge + ollama without deploying them in this harness. The
# in-cluster registry runs over plain HTTP, so signal the script to
# add `crane --insecure`. We push via the 127.0.0.1:5000 bind (host
# reachable) because the `opencost-ai-registry` hostname only
# resolves inside the kind docker network; pods still pull by the
# kind-network hostname (see kind-config.yaml).
if [[ "${MIRROR_UPSTREAM}" -eq 1 ]]; then
  echo "==> mirroring upstream bridge + ollama images"
  CRANE_INSECURE=1 "${REPO_ROOT}/scripts/air-gap/mirror-images.sh" \
    "ollama/ollama:0.6.0=localhost:${REGISTRY_PORT}/ollama:0.6.0" \
    "ghcr.io/jonigl/ollama-mcp-bridge:latest=localhost:${REGISTRY_PORT}/ollama-mcp-bridge:airgap-e2e"
fi

# Exercise the ORAS model-push path with a synthetic tiny GGUF. Real
# model weights would blow past CI's disk budget, but the mirror path
# and media types are the same regardless of blob size, so this is
# enough to catch a regression in the push/pull scripts.
if command -v oras >/dev/null 2>&1; then
  echo "==> ORAS push dry-run (synthetic model artefact)"
  MODEL_STAGE="$(mktemp -d)"
  dd if=/dev/urandom of="${MODEL_STAGE}/stub.gguf" bs=1024 count=8 status=none
  cat > "${MODEL_STAGE}/stub.Modelfile" <<MODELFILE
# Synthetic Modelfile for airgap e2e. Not a real model.
FROM ./stub.gguf
MODELFILE
  ORAS_PLAIN_HTTP=1 "${REPO_ROOT}/scripts/air-gap/oras-push-model.sh" \
    "localhost:${REGISTRY_PORT}/ollama-model/stub:airgap-e2e" \
    "${MODEL_STAGE}/stub.gguf"
  rm -rf "${MODEL_STAGE}"
else
  echo "==> oras not installed; skipping ORAS push dry-run (non-fatal)"
fi

# --- step 3: apply egress block -----------------------------------------

KIND_BRIDGE="$(docker network inspect kind -f '{{index .Options "com.docker.network.bridge.name"}}' 2>/dev/null || true)"
if [[ -z "${KIND_BRIDGE}" ]]; then
  KIND_NET_ID="$(docker network inspect kind -f '{{.Id}}')"
  KIND_BRIDGE="br-${KIND_NET_ID:0:12}"
fi
echo "==> kind bridge: ${KIND_BRIDGE}"

REGISTRY_CIDR="$(docker network inspect kind -f '{{(index .IPAM.Config 0).Subnet}}')"
# Order matters: iptables evaluates rules top-down and -I prepends,
# so the DROP goes in first and then the RETURN goes in front of it.
# After both inserts the effective order is RETURN-then-DROP, which
# is what we want.
iptables -I DOCKER-USER 1 \
  -i "${KIND_BRIDGE}" \
  -m comment --comment "${IPT_COMMENT}" \
  -j DROP
iptables -I DOCKER-USER 1 \
  -i "${KIND_BRIDGE}" \
  -d "${REGISTRY_CIDR}" \
  -m comment --comment "${IPT_COMMENT}" \
  -j RETURN
echo "    iptables: RETURN -d ${REGISTRY_CIDR}, DROP -i ${KIND_BRIDGE}"

# --- step 4: install the chart -----------------------------------------

echo "==> preparing namespace and auth token"
kubectl create namespace "${NAMESPACE}"
kubectl label namespace "${NAMESPACE}" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted

kubectl -n "${NAMESPACE}" create secret generic opencost-ai-auth \
  --from-literal=token="airgap-e2e-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"

echo "==> helm install opencost-ai (gateway-only profile)"
helm install opencost-ai "${REPO_ROOT}/deploy/helm/opencost-ai" \
  --namespace "${NAMESPACE}" \
  --values "${REPO_ROOT}/test/airgap/values-airgap.yaml" \
  --wait --timeout 5m

# --- step 5: assertions -------------------------------------------------

echo ""
echo "==> assertion 1: gateway /v1/health returns status:ok from inside the namespace"
# In-cluster curl proves the Service + NetworkPolicy ingress wiring,
# not just pod readiness.
kubectl -n "${NAMESPACE}" run probe-health \
  --image=curlimages/curl:8.10.1 \
  --restart=Never \
  --image-pull-policy=IfNotPresent \
  --labels=app.kubernetes.io/component=airgap-probe \
  --command -- sh -c '
    for i in 1 2 3 4 5 6 7 8 9 10; do
      if body=$(curl -fsS --max-time 5 http://opencost-ai-gateway:8080/v1/health); then
        echo "${body}"
        echo "${body}" | grep -q "\"status\":\"ok\"" && exit 0
      fi
      sleep 2
    done
    echo "gateway /v1/health never returned status:ok" >&2
    exit 1
  '
kubectl -n "${NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/probe-health --timeout=60s
kubectl -n "${NAMESPACE}" logs probe-health
kubectl -n "${NAMESPACE}" delete pod probe-health --wait=false

echo ""
echo "==> assertion 2: internet egress from the namespace is blocked"
# Load-bearing assertion. Curl must NOT reach the public internet.
kubectl -n "${NAMESPACE}" run probe-egress \
  --image=curlimages/curl:8.10.1 \
  --restart=Never \
  --image-pull-policy=IfNotPresent \
  --labels=app.kubernetes.io/component=airgap-probe \
  --command -- sh -c '
    if curl --max-time 5 -sS -o /dev/null https://1.1.1.1 2>/dev/null; then
      echo "EGRESS LEAK: curl to 1.1.1.1 succeeded; air-gap is theatrical" >&2
      exit 1
    fi
    echo "egress blocked (expected)"
    exit 0
  '
kubectl -n "${NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/probe-egress --timeout=30s
kubectl -n "${NAMESPACE}" logs probe-egress
kubectl -n "${NAMESPACE}" delete pod probe-egress --wait=false

echo ""
echo "==> assertion 3: in-cluster registry is still reachable (block is scoped)"
# If the block were overly broad, ImagePullBackOff would have tripped
# step 4. Double-check explicitly so a future tightening does not
# silently turn into a DoS of the registry path.
kubectl -n "${NAMESPACE}" run probe-registry \
  --image=curlimages/curl:8.10.1 \
  --restart=Never \
  --image-pull-policy=IfNotPresent \
  --labels=app.kubernetes.io/component=airgap-probe \
  --command -- sh -c "
    if ! curl --max-time 5 -fsS http://${REGISTRY_NAME}:${REGISTRY_PORT}/v2/ >/dev/null; then
      echo 'registry unreachable from inside the namespace' >&2
      exit 1
    fi
    echo 'registry reachable (expected)'
    exit 0
  "
kubectl -n "${NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/probe-registry --timeout=30s
kubectl -n "${NAMESPACE}" logs probe-registry
kubectl -n "${NAMESPACE}" delete pod probe-registry --wait=false

echo ""
echo "==> assertion 4: gateway pod image came from the in-cluster registry"
# Cross-check: the Deployment's image reference must point at the
# internal registry, not at ghcr.io. A silent fallback to the public
# source would still work if the egress block were misapplied, and
# nothing above catches that.
image_ref=$(kubectl -n "${NAMESPACE}" get deploy opencost-ai-gateway \
  -o jsonpath='{.spec.template.spec.containers[0].image}')
case "${image_ref}" in
  ${REGISTRY_NAME}:${REGISTRY_PORT}/*)
    echo "image ref: ${image_ref} (ok)"
    ;;
  *)
    echo "UNEXPECTED: gateway image reference is ${image_ref}, expected ${REGISTRY_NAME}:${REGISTRY_PORT}/*" >&2
    exit 1
    ;;
esac

echo ""
echo "============================================================"
echo "  AIR-GAP E2E PASSED"
echo "  gateway reachable:       yes (image from in-cluster registry)"
echo "  egress to 1.1.1.1:       blocked"
echo "  registry reachable:      yes"
echo "  mirror upstream:         ${MIRROR_UPSTREAM}"
echo "============================================================"
