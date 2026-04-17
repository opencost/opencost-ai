# opencost-ai Helm chart

> **Status: pre-v0.1, pre-release.** Chart is scaffolded but the
> referenced `ghcr.io/opencost/opencost-ai-gateway` image has not yet
> been published. Treat this directory as the install contract the
> gateway ships against, not a production-ready install.

Installs the opencost-ai stack into a Kubernetes cluster:

- **gateway** — the thin Go HTTP API in this repo
  (`cmd/gateway`)
- **bridge** — upstream
  [`jonigl/ollama-mcp-bridge`](https://github.com/jonigl/ollama-mcp-bridge)
- **ollama** — upstream [Ollama](https://ollama.com/) with a PVC for
  the model cache

A single release, three Deployments/StatefulSets, three
NetworkPolicies enforcing the "no internet egress" posture documented
in `docs/architecture.md` §7.5, and an optional ServiceMonitor for
Prometheus Operator users.

## Topology enforced by the chart

```
client  →  gateway  →  bridge  →  ollama
                             \→  opencost-mcp (:8081)
```

NetworkPolicies restrict egress to exactly this shape. No component is
granted egress to the public internet or to any in-cluster service
not listed above.

| Component | Allowed egress                                              |
| --------- | ----------------------------------------------------------- |
| gateway   | bridge service, kube-dns                                    |
| bridge    | ollama service, OpenCost MCP service, kube-dns              |
| ollama    | kube-dns only (model weights must already be on the PVC)    |

## Requirements

- Kubernetes ≥ 1.25 (for the `pod-security.kubernetes.io/enforce`
  label taking effect at namespace admission time)
- A CNI that enforces NetworkPolicy (Calico, Cilium, Antrea). Flannel
  without an overlay will render the policies but not enforce them.
- A StorageClass with `ReadWriteOnce` support for the Ollama PVC, or
  a pre-provisioned volume.
- OpenCost ≥ v1.118 running in the cluster. The built-in MCP server
  on port 8081 is what the bridge talks to.

## Install

```sh
# 1. Create a namespace labelled for PodSecurity restricted. The chart's
#    pod and container securityContexts are already compliant; the
#    label makes admission enforce it.
kubectl create namespace opencost-ai
kubectl label namespace opencost-ai \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted

# 2. Create a bearer-token Secret. The chart refuses to ship a default.
#    Generate a 32-byte token and store it under the key `token`:
kubectl -n opencost-ai create secret generic opencost-ai-auth \
  --from-literal=token="$(openssl rand -hex 32)"

# 3. Install the chart pointing at the Secret you created.
helm install opencost-ai ./deploy/helm/opencost-ai \
  --namespace opencost-ai \
  --set gateway.auth.existingSecret=opencost-ai-auth
```

### Alternative: let the chart create the Secret

For dev clusters where dropping the token into the install command is
acceptable:

```sh
helm install opencost-ai ./deploy/helm/opencost-ai \
  --namespace opencost-ai \
  --set gateway.auth.create=true \
  --set gateway.auth.token="$(openssl rand -hex 32)"
```

Do not commit a values file containing `gateway.auth.token` to any
repository. Per `CLAUDE.md` non-negotiables, the chart never writes a
token default and never ships a Secret unless `create: true` is
explicitly set alongside a supplied token value.

## Verify the install

```sh
# Port-forward the gateway Service.
kubectl -n opencost-ai port-forward \
  svc/opencost-ai-gateway 8080:8080 &

# /v1/health is unauthenticated (liveness). If this 200s, the gateway
# is up; it does not prove the bridge or Ollama are ready.
curl -sS http://127.0.0.1:8080/v1/health

# /v1/ask requires the bearer token.
TOKEN=$(kubectl -n opencost-ai get secret opencost-ai-auth \
          -o jsonpath='{.data.token}' | base64 -d)
curl -sS -H "Authorization: Bearer ${TOKEN}" \
     -H "Content-Type: application/json" \
     -d '{"query":"what did i spend yesterday?"}' \
     http://127.0.0.1:8080/v1/ask
```

## Prometheus ServiceMonitor

If you run Prometheus Operator, enable the ServiceMonitor:

```sh
helm upgrade --install opencost-ai ./deploy/helm/opencost-ai \
  --namespace opencost-ai \
  --set gateway.serviceMonitor.enabled=true
```

The ServiceMonitor renders only when the target cluster exposes the
`monitoring.coreos.com/v1` API, so clusters without the operator see a
no-op on the flag rather than an apply failure.

## Air-gap installation

The chart expects images and model weights to already be reachable
from inside the cluster. The end-to-end flow (GGUF export, OCI push,
`ollama create`) lands in `docs/air-gap-install.md` in a follow-up
PR; `docs/architecture.md` §10 decision 3 records the transport
decision (OCI via ORAS).

Image mirroring checklist:

1. Pull the four images on a connected host:
   - `ghcr.io/opencost/opencost-ai-gateway:<appVersion>`
   - `ghcr.io/jonigl/ollama-mcp-bridge:<tag>`
   - `ollama/ollama:<tag>`
2. Re-tag and push to your internal registry.
3. Override the repositories via `--set
   {gateway,bridge,ollama}.image.repository=...` at install time.
4. Pre-load the Ollama model cache onto the PVC (via an init job or a
   pre-populated volume) so no `ollama pull` is attempted at runtime —
   the Ollama NetworkPolicy blocks egress beyond kube-dns on purpose.

## Values reference

See `values.yaml` for the full schema. Every knob carries an inline
comment describing its effect.

| Key                                          | Default                               | Purpose                                                                |
| -------------------------------------------- | ------------------------------------- | ---------------------------------------------------------------------- |
| `gateway.image.repository`                   | `ghcr.io/opencost/opencost-ai-gateway`| Override for air-gap / private mirrors                                 |
| `gateway.config.defaultModel`                | `qwen2.5:7b-instruct`                 | Default Ollama model; `mistral-nemo:12b` is the documented upgrade     |
| `gateway.config.auditLogQuery`               | `false`                               | **Do not flip without review** — opt-in to query capture in audit log  |
| `gateway.auth.existingSecret`                | `""`                                  | Name of a pre-created Secret holding the bearer token                  |
| `gateway.auth.create`                        | `false`                               | Render a chart-managed Secret; requires `gateway.auth.token`           |
| `gateway.serviceMonitor.enabled`             | `false`                               | Render ServiceMonitor if Prometheus Operator is installed              |
| `networkPolicy.metricsIngress.allowedFrom`   | `[{podSelector: {}}]` (same-namespace)| Who may scrape the unauthenticated `/metrics` listener                 |
| `bridge.opencostMcp.host` / `port`           | `opencost.opencost.svc…` / `8081`     | In-cluster address of the OpenCost MCP server                          |
| `ollama.persistence.size`                    | `20Gi`                                | PVC size; scale with the model count and size                          |
| `ollama.persistence.storageClassName`        | `""`                                  | Cluster default SC when empty; set explicitly on air-gap clusters      |
| `networkPolicy.enabled`                      | `true`                                | Global toggle; turn off only for debugging                             |
| `networkPolicy.gatewayIngress.allowedFrom`   | `[]`                                  | Tighten gateway ingress beyond the same-namespace default              |

## Uninstall

```sh
helm uninstall opencost-ai -n opencost-ai
# PVC is retained by default so model weights survive a reinstall.
kubectl -n opencost-ai delete pvc -l app.kubernetes.io/component=ollama
kubectl delete namespace opencost-ai
```

## Chart development

- Lint:      `helm lint deploy/helm/opencost-ai`
- Template:  `helm template opencost-ai deploy/helm/opencost-ai > /tmp/rendered.yaml`
- CI:        `.github/workflows/helm.yml` runs lint, template, and a
             kind-based integration test on every PR that touches
             `deploy/helm/**` or the gateway sources.

Do not add subcharts without discussion; the flat layout is a
deliberate choice for a v0.1 chart whose three components are tightly
coupled by the enforced NetworkPolicy topology.
