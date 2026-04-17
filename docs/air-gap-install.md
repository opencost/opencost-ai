# Air-gap install

> This document is the executable contract for the feature that
> justifies this project. The whole reason `opencost-ai` exists, rather
> than "point Claude at your OpenCost data," is that regulated and
> sovereign environments cannot ship cluster cost data across a
> corporate perimeter. Everything here assumes a cluster with **zero**
> egress to the public internet.

## Scope

This flow takes a freshly-built cluster with no outbound internet
access, a registry reachable only from inside the perimeter, and the
four artefacts listed below, and produces a working `opencost-ai`
install:

1. The **gateway** image (`ghcr.io/opencost/opencost-ai-gateway`) built
   from this repo.
2. The **bridge** image (`ghcr.io/jonigl/ollama-mcp-bridge`), upstream.
3. The **Ollama** runtime image (`ollama/ollama`), upstream.
4. The **model weights** (`qwen2.5:7b-instruct` by default), packaged
   as a GGUF file wrapped in an OCI artefact.

The output is a `helm install` against the chart in
`deploy/helm/opencost-ai/` with every `image.repository` value
overridden to the internal registry, and the Ollama PVC pre-populated
with the default model.

## Topology

```
+---------------------+     +-----------------------+     +-----------------+
|  Connected host     | --> | Internal OCI registry | --> | Air-gap cluster |
|  (staging machine)  |     | (inside perimeter)    |     | (no egress)     |
+---------------------+     +-----------------------+     +-----------------+
         |                             ^                          |
         |  ORAS push (GGUF)           |                          |
         |  crane push (images)        |                          |
         +-----------------------------+                          |
                                                                  |
                  helm install --set image.repository=...  --------+
```

Three rules the design enforces:

- **No runtime `ollama pull`.** The Ollama NetworkPolicy whitelists
  only kube-dns. If a manifest reaches the cluster and tries to pull a
  model at runtime, it will fail closed. Weights must be on the PVC
  before the Ollama pod starts.
- **No registry indirection at runtime.** Helm `image.repository` is
  overridden at install time; pods never reference the original
  `ghcr.io` or `docker.io` paths.
- **Model distribution uses the same registry auth as images.** Model
  weights ride the OCI protocol via ORAS so operators reuse the
  registry mirror / cosign / pull-secret infrastructure they already
  operate.

## Prerequisites

### On the connected staging host

- `ollama` ≥ 0.6.0 (for the default model tag line)
- `oras` ≥ 1.2
- `crane` (go-containerregistry) for lossless image copy
- `cosign` ≥ 2.4 (optional, recommended for verification pre-push)
- `helm` ≥ 3.16 and `kubectl` ≥ 1.31 (for the dry-run render below)
- ~20 GB free disk (weights + image layers)

### On the disconnected cluster

- Kubernetes ≥ 1.25 with a CNI that **enforces** NetworkPolicy (Calico,
  Cilium, Antrea). Flannel without an overlay renders policies but
  does not enforce them — the air-gap posture degrades silently.
- A StorageClass backing the Ollama PVC (ReadWriteOnce is enough for
  the default single-replica StatefulSet).
- An internal OCI registry reachable from every node. Path-addressable
  (e.g. Harbor, Zot, ECR / ACR / GAR in-VPC, or a plain `registry:2`
  behind mTLS) — what matters is that `containerd` can pull from it
  and `oras pull` works against it with the same credentials.
- OpenCost ≥ v1.118 already installed (this chart is a consumer of its
  built-in MCP server on port 8081).

## Step 1 — Stage the model on the connected host

The goal is to produce two artefacts:

- A GGUF file on disk.
- An OCI artefact in the internal registry that wraps that GGUF.

`ollama pull` downloads a manifest-plus-blobs tree into `~/.ollama`.
We re-use the existing blob layout: Ollama's model storage is already
a content-addressable store, so the GGUF is a single file we can point
ORAS at.

```sh
# 1. Pull on a machine with internet access.
ollama pull qwen2.5:7b-instruct

# 2. Locate the GGUF blob that backs the manifest. Ollama stores blobs
#    under $HOME/.ollama/models/blobs/ as sha256-<hex> filenames on
#    disk (the colon from the digest is replaced with a dash); the
#    manifest under $HOME/.ollama/models/manifests/ references the
#    layer whose mediaType is application/vnd.ollama.image.model.
./scripts/air-gap/export-gguf.sh qwen2.5:7b-instruct ./stage/qwen2.5-7b-instruct.gguf

# 3. Push the GGUF to the internal registry as an OCI artefact. The
#    reference uses an `ollama-model/` path prefix by convention so
#    registry admins can scope policies separately from container
#    images.
./scripts/air-gap/oras-push-model.sh \
  registry.internal.example/ollama-model/qwen2.5-7b-instruct:latest \
  ./stage/qwen2.5-7b-instruct.gguf
```

`scripts/air-gap/oras-push-model.sh` tags the artefact with the
`application/vnd.ollama.image.model` media type — the same type Ollama
itself uses, so the artefact round-trips through Ollama tooling later.
It also attaches the source `Modelfile` as a separate layer so the
cluster side has everything needed to run `ollama create` without
inventing parameters.

The artefact layout on the registry:

```
manifest (application/vnd.oci.image.manifest.v1+json)
├── config: application/vnd.ollama.image.config+json
├── layer 0: application/vnd.ollama.image.model        (GGUF bytes)
└── layer 1: application/vnd.ollama.image.modelfile    (Modelfile)
```

## Step 2 — Mirror the container images

Three container images, one command each. `crane copy` preserves the
manifest digest, which matters when you later pin
`image.digest` in `values.yaml` (recommended for reproducible
installs).

```sh
SRC_GATEWAY=ghcr.io/opencost/opencost-ai-gateway:v0.1.0
SRC_BRIDGE=ghcr.io/jonigl/ollama-mcp-bridge:v0.2.0
SRC_OLLAMA=ollama/ollama:0.6.0

DST=registry.internal.example/opencost-ai

./scripts/air-gap/mirror-images.sh \
  "${SRC_GATEWAY}=${DST}/opencost-ai-gateway:v0.1.0" \
  "${SRC_BRIDGE}=${DST}/ollama-mcp-bridge:v0.2.0" \
  "${SRC_OLLAMA}=${DST}/ollama:0.6.0"
```

The script uses `crane copy` and prints the destination digest for
each image. Record those digests — they feed the `image.digest`
values below. Digest pinning is the belt-and-braces answer to "how do
I know the registry admin did not silently retag?".

If the staging host cannot reach the internal registry directly (i.e.
staging is doubly-isolated), replace the single-step copy with a
two-step `crane pull` → sneakernet tarball → `crane push` flow. The
script supports this via the `OCI_LAYOUT` and `AIRGAP_LAYOUT_MODE`
environment variables (`pull` on the connected side, `push` on the
disconnected side); set `CRANE_INSECURE=1` if the destination
registry serves plain HTTP.

## Step 3 — Pre-populate the Ollama PVC

Two supported approaches. Pick one based on how your cluster handles
volume provisioning.

### 3a. Init job (default)

A one-shot Job mounts the Ollama PVC, pulls the OCI artefact produced
in Step 1, materialises the GGUF to the PVC, and runs `ollama create`
against a local Ollama binary (bundled in the Ollama image) to
register the model under the expected tag. The Job completes before
the Ollama StatefulSet starts.

```sh
helm upgrade --install opencost-ai deploy/helm/opencost-ai \
  --namespace opencost-ai \
  --values values-airgap.yaml \
  --set ollama.modelBootstrap.enabled=true \
  --set ollama.modelBootstrap.ociRef=registry.internal.example/ollama-model/qwen2.5-7b-instruct:latest \
  --set ollama.modelBootstrap.modelName=qwen2.5:7b-instruct
```

The init Job borrows the Ollama image (it already has the `ollama`
binary) plus a tiny `oras` sidecar. Both images must be mirrored per
Step 2.

> **Status:** the `ollama.modelBootstrap.*` values and the template
> that renders the Job are tracked in
> [issue TBD](https://github.com/opencost/opencost-ai/issues). Until
> that ships, use approach 3b.

### 3b. Pre-provisioned volume

For shops that provision volumes out of band (NetApp / PowerScale /
pre-baked CSI snapshots), skip the Job entirely:

```sh
# On a helper pod with the Ollama PVC mounted at /var/lib/ollama and
# the `ollama` binary in PATH (any image based on ollama/ollama works).
# The helper script locates the actual `*.Modelfile` filename inside
# the pulled artefact (the push side preserves the operator-supplied
# basename) and synthesises one if the artefact has none, so this
# does not break when the source filename was, say,
# `qwen2.5-7b-instruct.Modelfile`.
scripts/air-gap/oras-pull-model.sh \
  registry.internal.example/ollama-model/qwen2.5-7b-instruct:latest \
  qwen2.5:7b-instruct
# Ollama writes the registered model into $HOME/.ollama which HOME is
# relocated to /var/lib/ollama by the StatefulSet (see
# deploy/helm/opencost-ai/templates/ollama-statefulset.yaml).
```

Either way, verify the model is on the PVC before installing the
chart:

```sh
kubectl -n opencost-ai exec pod/ollama-model-loader -- \
  ollama list | grep qwen2.5:7b-instruct
```

## Step 4 — Install the chart with mirrored images

Create a `values-airgap.yaml` in your GitOps repo (not this repo —
air-gap coordinates are site-specific):

```yaml
# values-airgap.yaml
imagePullSecrets:
  - name: internal-registry

gateway:
  image:
    repository: registry.internal.example/opencost-ai/opencost-ai-gateway
    tag: v0.1.0
    digest: sha256:...    # paste from Step 2 output
    pullPolicy: IfNotPresent

bridge:
  image:
    repository: registry.internal.example/opencost-ai/ollama-mcp-bridge
    tag: v0.2.0
    digest: sha256:...

ollama:
  image:
    repository: registry.internal.example/opencost-ai/ollama
    tag: "0.6.0"
    digest: sha256:...
  persistence:
    storageClassName: "internal-block"    # explicit, not default

networkPolicy:
  # The default policies already block internet egress. Nothing to do
  # here in an air-gap install — this section documents the invariant.
  enabled: true
```

Install:

```sh
kubectl create namespace opencost-ai
kubectl label namespace opencost-ai \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted

kubectl -n opencost-ai create secret generic opencost-ai-auth \
  --from-literal=token="$(openssl rand -hex 32)"

helm install opencost-ai ./deploy/helm/opencost-ai \
  --namespace opencost-ai \
  --values values-airgap.yaml \
  --set gateway.auth.existingSecret=opencost-ai-auth \
  --wait --timeout 5m
```

## Step 5 — Verify

From a pod inside the cluster (not from a port-forward — we want to
exercise the NetworkPolicy):

```sh
kubectl -n opencost-ai run curl --rm -it --restart=Never \
  --image=registry.internal.example/opencost-ai/curl:8.10 -- \
  sh -c '
    TOKEN=$(cat /var/run/secrets/opencost-ai/token)
    curl -fsS -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{\"query\":\"what did i spend yesterday?\"}" \
      http://opencost-ai-gateway.opencost-ai.svc:8080/v1/ask
  '
```

A successful response proves:

- The gateway image pulled from the internal registry.
- The bridge reached Ollama on the private `ClusterIP`.
- Ollama found the model on the PVC (no pull attempt, because the
  NetworkPolicy would have blocked one).
- The bridge reached the OpenCost MCP server on :8081.

### Egress sanity check

Confirm nothing in the namespace can reach the internet. This is the
single most important assertion — if it fails, the install is a
security regression regardless of whether the happy path works:

```sh
kubectl -n opencost-ai debug -it \
  deploy/opencost-ai-gateway \
  --image=registry.internal.example/opencost-ai/curl:8.10 \
  --target=gateway -- \
  sh -c 'curl --max-time 5 -sS https://1.1.1.1 || echo "egress blocked (expected)"'
```

Expected: `egress blocked (expected)`. Anything else means the
NetworkPolicy did not render, the CNI does not enforce it, or a
sidecar is leaking.

## End-to-end test harness

`test/airgap/run.sh` automates a reduced version of Steps 1–5 against
a disposable kind cluster. It is the **real** validation for this
feature — not a render-only check — and runs in CI on the
`airgap-e2e` workflow (see `.github/workflows/airgap-e2e.yml`).

The harness:

1. Boots a normal disposable kind cluster (no default-route surgery —
   the egress block is enforced at the host firewall, not via kind
   networking).
2. Stands up an in-cluster `registry:2` attached to the `kind` docker
   network.
3. Builds the gateway image and pushes it into the in-cluster
   registry via `docker push` (against the registry's
   `127.0.0.1:5000` bind). Optionally (`--mirror-upstream`) also
   `crane copy`s real upstream `ollama/ollama` and
   `ghcr.io/jonigl/ollama-mcp-bridge` images into the registry to
   exercise the mirror path for the full stack. Also runs
   `scripts/air-gap/oras-push-model.sh` against a synthetic few-KB
   GGUF when `oras` is on PATH, so a regression in the ORAS push/pull
   scripts is caught by the same job.
4. Applies two `iptables` rules on the host's `DOCKER-USER` chain:
   one RETURN rule for packets destined to the kind CIDR (so the
   registry stays reachable), and a DROP rule for everything else
   leaving the kind bridge. Rules are tagged with a comment so the
   `EXIT` trap cleans them up even on partial failure.
5. Installs the chart with `gateway.image.repository` pointing at the
   in-cluster registry, PodSecurity `restricted` enforced on the
   namespace, and the bridge/ollama components disabled (see
   `test/airgap/README.md` for why the full stack is out of scope
   here).
6. Runs three probe pods:
   - `probe-health` exits 0 if `/v1/health` returns `status:ok`.
   - `probe-egress` exits 0 if `curl https://1.1.1.1` **fails**.
   - `probe-registry` exits 0 if the in-cluster registry is
     reachable.
7. Cross-checks the Deployment's image reference against the
   in-cluster registry path so a silent fallback to `ghcr.io` cannot
   pass.
8. Tears down kind and removes the iptables rules.

Run it locally:

```sh
sudo test/airgap/run.sh
```

It requires `sudo` because the iptables rule is a host-level change —
this is the honest cost of "really blocked" vs. "looks blocked." An
earlier no-sudo mode based on Docker network isolation was removed
after review; see `test/airgap/README.md` for why a netns-only block
was not honest enough to keep.

## Operational notes

### Updating the model

Replace Step 1's artefact in the internal registry and re-run the
init Job (Step 3a). The Job is idempotent — `ollama create` on an
existing tag is a no-op unless the Modelfile changed. Models are
content-addressed on the PVC, so rolling back is `helm upgrade` with
the previous `ollama.modelBootstrap.ociRef` value.

### Rotating the registry credentials

The chart reads pull credentials from the `imagePullSecrets` list.
Rotate the Secret, let the Deployment roll; the ORAS-based model
artefacts use the same credentials as image pulls (both go through
the Docker config file path) so rotating once covers both.

### GPU nodes

Nothing in this flow assumes CPU-only inference. If the Ollama pod
schedules on a GPU node, the `ollama/ollama` image already contains
the CUDA bits; the model cache is hardware-independent. Add the
standard `nvidia.com/gpu: 1` resource request under
`ollama.resources.limits` in your values file.

### Licensing

`qwen2.5:7b-instruct`, `mistral-nemo:12b`, and `llama3.1:8b-instruct`
are all redistributable under their respective licences (Apache 2.0,
Apache 2.0, and Meta Llama 3 Community License) — shipping the weights
into a private registry is covered by each. Teams pushing other
models are responsible for confirming the licence terms of their
chosen weights before mirroring.

### What is explicitly not supported in v0.1

- `ollama pull` at runtime against an internal Ollama Registry. The
  upstream registry protocol is not stable enough to target as a
  first-class mirror. The OCI+ORAS path here is deliberately
  registry-agnostic.
- Fine-tuned adapters (LoRA). The cache path exists in Ollama but the
  chart does not template a values schema for them yet.
- Cross-registry replication. The chart reads one repository per
  image; multi-region replication is the registry's job, not the
  chart's.

## Reference — resolved decisions

Cross-reference with `docs/architecture.md` §10:

- Decision 3: **Model weights in air-gap: OCI registry via ORAS.**
  This doc is the concrete implementation of that decision.
- Decision 1/2: **MCP transport is `streamable_http`.** The bridge
  config rendered by the chart already uses this, no air-gap-specific
  change.
- Decision 5: **Default model is `qwen2.5:7b-instruct`.** The VRAM
  floor (~6 GB for 7B-Q4, ~10 GB for `mistral-nemo:12b`) determines
  node sizing on the cluster side and Helm `ollama.resources.limits`
  values.

If this document and `docs/architecture.md` disagree, the architecture
doc wins and this one needs a fix in the same PR that makes them
agree.
