# Operations Guide

How to deploy and configure the orchestrator. Consumers of the API want the [jobs](jobs.md), [deployments](deployments.md), and [pools](pools.md) guides instead.

- [What gets deployed](#what-gets-deployed)
- [Prerequisites](#prerequisites)
- [Installing](#installing)
- [Enabling deployments](#enabling-deployments)
- [Pools](#pools)
- [Configuration reference](#configuration-reference)
- [Hardening](#hardening)
- [Resource model](#resource-model)
- [Scaling the control plane](#scaling-the-control-plane)
- [Local development backend](#local-development-backend)

## What gets deployed

The Helm chart (`charts/orchestrator/`) installs up to two stateless services:

Every component is opt-in — a default install renders nothing.

| Service | Enable with | Serves |
| --- | --- | --- |
| **jobs** | `jobs.enabled` | `/v1/jobs` — run-to-completion workloads (`batch/v1.Job` + a native sidecar per job) |
| **deployments** | `deployments.enabled` | `/v1/deployments` and `/v1/deployment-pools` — long-lived HTTP workloads and warm pools |
| **activator** | `deployments.activator.enabled` | The buffering edge for cold and async traffic — required for scale-to-zero and `Prefer: respond-async` |

Both derive all state from the cluster: restarts and replica failovers lose nothing, and there is no database.

## Prerequisites

- **Kubernetes 1.29+** — jobs rely on native sidecar containers.
- **Helm 3.8+** — the chart is published as an OCI artifact.
- For deployments: the **Gateway API CRDs** and a gateway controller with two *Extended* features — per-backendRef `RequestHeaderModifier` filters and `RegularExpression` header matching. Verified on **Traefik** (Gateway API v1.5+); Envoy Gateway and Istio also implement both. Without a gateway, set `deployments.gateway.enabled=false`: deployments still run, but you provide your own routing.

## Installing

```bash
helm install orchestrator oci://ghcr.io/open-runtimes/charts/orchestrator \
  --version <X.Y.Z> \
  --namespace orchestrator --create-namespace \
  --set jobs.enabled=true
```

This deploys the jobs service alone (components are opt-in; a bare install renders nothing). A complete worked example — serving plane, hardened workload namespace, pools, HA — lives at [`examples/helm-values.yaml`](../examples/helm-values.yaml). Verify with:

```bash
kubectl -n orchestrator port-forward svc/jobs 8080:8080 &
curl localhost:8080/readyz
```

To require authentication, mount a secret and point `API_KEY_FILE` at it (via `extraEnv` + a volume, or your secrets operator). Requests then need `Authorization: Bearer <key>`. **With no key configured, the API is open** — the services log a warning at startup.

## Enabling deployments

```yaml
# values.yaml
deployments:
  enabled: true
  activator:
    enabled: true # the cold/async buffering edge
  domain: apps.example.com        # auto-assigned hosts become {id}.apps.example.com
  gateway:
    name: orchestrator            # the Gateway resource HTTPRoutes attach to
    namespace: ""                 # empty = release namespace
```

You own the `Gateway` resource (listener config, TLS, load balancer); the orchestrator writes one `HTTPRoute` per deployment against it:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: orchestrator
  namespace: orchestrator
spec:
  gatewayClassName: traefik
  listeners:
    - name: http
      port: 80
      protocol: HTTP
      allowedRoutes:
        namespaces: { from: All }   # required when workloadNamespace is enabled
```

Point a wildcard DNS record (`*.apps.example.com`) at the gateway's address and deployments are reachable the moment they're ready.

## Pools

Pools are declared in values — adding, resizing, or removing one is a values change and rollout, not an API call:

```yaml
deployments:
  pools:
    - id: py
      image: python:3.12-slim
      size: 4                # warm pods kept ready
      cpu: 1
      memory: 512
      burst: reject          # reject (429) | cold (create on demand)
    - id: node
      image: node:22-slim
      size: 2
      port: 3000             # >0 makes it an HTTP pool
      burst: cold
```

Each pool keeps `size` pods warm at all times; claimed pods are replaced off the request path. See the [pools guide](pools.md) for the consumer side.

## Configuration reference

The most consequential values (see `charts/orchestrator/values.yaml` for the full annotated set):

| Value | Default | Effect |
| --- | --- | --- |
| `jobs.image`, `jobs.sidecarImage` | GHCR latest | Jobs service and per-job sidecar images |
| `deployments.enabled` | `false` | Install the deployments service + activator |
| `deployments.domain` | `localhost` | Base domain for auto-assigned hosts |
| `deployments.gateway.{enabled,name,namespace}` | `true`, `orchestrator`, release ns | The Gateway that HTTPRoutes attach to |
| `deployments.dataPort` | `8081` | Docker-backend data plane / activator data port |
| `deployments.pools` | `[]` | Warm pool declarations (above) |
| `deployments.workloadNamespace.*` | disabled | Hardened namespace for workload pods (below) |
| `deployments.cpuOvercommit` | `1` | CPU request = limit / overcommit (below) |
| `deployments.leaderElection.enabled` | `false` | Required when `deployments.replicaCount > 1` |
| `deployments.limitRange.enabled` | `false` | Default requests for unspecified containers |
| `deployments.activator.replicaCount` | `1` | Buffering-edge replicas |
| `service.apiPort` / `service.metricsPort` | `8080` / `9090` | API and Prometheus ports |
| `extraEnv` | `[]` | Extra environment for the services (e.g. `AUTOSCALER_WINDOW`, `API_KEY_FILE`) |

Autoscaler tuning via `extraEnv`: `AUTOSCALER_WINDOW` (sliding window, default 60s) and `AUTOSCALER_TICK` (evaluation period, default 2s). A shorter window scales — in both directions — more aggressively.

## Hardening

Defense in depth, each layer independently optional:

**Pod security floor (always on).** Every workload pod runs as non-root (uid 65532) with all capabilities dropped, `RuntimeDefault` seccomp, a read-only root filesystem, and no ServiceAccount token — admissible under Pod Security Standards `restricted`.

**Dedicated workload namespace.** With `deployments.workloadNamespace.enabled=true`, workload pods (deployments and pools) run in their own namespace (default `{release}-workloads`), separated from the control plane:

```yaml
deployments:
  workloadNamespace:
    enabled: true
    podSecurity: restricted        # PSA enforce/audit/warn labels
    networkPolicy:
      enabled: true                # default-deny ingress; egress blocks 169.254.169.254
      gatewayNamespace: traefik-system
    resourceQuota:
      enabled: true
      pods: 200
      requestsCpu: "100"
      requestsMemory: 200Gi
```

The NetworkPolicy admits ingress only from the gateway, the activator, and the control plane, and blocks egress to the cloud metadata endpoint — the highest-value single rule against SSRF credential theft. It requires an enforcing CNI (Cilium, Calico; kindnet does not enforce).

**Sandbox tiers.** Workloads can request stronger kernel isolation with `"sandbox": "gvisor"` or `"kata"` in their spec. Map tiers to your cluster's RuntimeClasses with `KUBE_SANDBOX_RUNTIME_CLASSES` (e.g. `gvisor=gvisor,kata=kata-qemu` via `extraEnv`); the service validates the RuntimeClass exists before accepting the workload, so a missing runtime is a `400`, not a stuck pod.

**Secrets at rest.** Deployment specs — including callback signing keys — are stored in Secrets, not ConfigMaps or annotations. Pool claim tokens are HMAC-derived per pod from an install key that never leaves its Secret.

## Tenant isolation

Clients can place workloads in per-tenant namespaces via the `tenant` field (jobs today; deployments and pools follow). Enable it per plane:

```yaml
jobs:
  enabled: true
  tenants:
    enabled: true   # grants the jobs service cluster-scoped job/pod access
                    # + namespace/serviceaccount create (on-demand provisioning)
```

With it on, a job's `tenant` (an RFC-1123 label) resolves to namespace `{workload-namespace}-{tenant}`, which the service creates on first use with restricted Pod Security admission labels. Off (the default), a non-empty `tenant` is rejected with `400`, and the service keeps its namespaced Role.

Tenant namespaces are deliberately cheap: **no per-namespace NetworkPolicy or ResourceQuota**. Network isolation instead comes from one cluster-wide policy spanning every workload namespace, and resource control from pod limits:

```yaml
# Requires Cilium; supply your own on other CNIs.
workloadNetworkPolicy:
  enabled: true    # CiliumClusterwideNetworkPolicy: DNS allowed,
                   # cloud metadata endpoint blocked, all else permitted
```

## Resource model

- **CPU**: workloads get a CPU *request* of `cpu / cpuOvercommit` and **no CPU limit** — CPU is compressible, and limits cause needless throttling. Raising `cpuOvercommit` packs workloads denser at the cost of contention under load.
- **Memory**: request = limit (incompressible; overcommitting memory trades OOM kills for density).
- Replicas spread across nodes via topology spread constraints, and durably multi-replica deployments get a PodDisruptionBudget automatically.
- `limitRange.enabled` adds defaults for containers that declare nothing, preventing BestEffort pods.

## Scaling the control plane

Set `deployments.replicaCount > 1` **and** `deployments.leaderElection.enabled=true`: all replicas serve the API (any replica can answer anything — state lives in the cluster), while the background reconcilers (rollout auto-cut, endpoint flip, autoscaler, pool control) run on the elected leader only. The activator scales independently via `deployments.activator.replicaCount`. The chart **fails fast at render time** if you ask for multiple replicas without leader election.

Each component can also autoscale on CPU (`autoscaling.enabled`, `deployments.autoscaling.enabled`, `deployments.activator.autoscaling.enabled` — min/max replicas and a target utilization); the services require leader election, the activator doesn't. Any component that is durably multi-replica (fixed count > 1, or an HPA) automatically gets a **PodDisruptionBudget** (`maxUnavailable: 1`) — never on singletons, where a PDB would block node drains.

The control-plane pods ship hardened by default: non-root with all capabilities dropped, `RuntimeDefault` seccomp, read-only root filesystems, zero-downtime surge rollouts, soft topology spread across nodes, and the same no-CPU-limit resource shape as workloads (memory capped, CPU uncapped).

## Local development backend

Both services also run against Docker (`ORCHESTRATOR_BACKEND=docker`) for development: `docker compose up -d` at the repo root brings up both against your local daemon (see [`docker-compose.yaml`](../docker-compose.yaml)), and `task dev` runs the jobs service from source with hot reload. The Docker backend is functionally reduced (single-revision deployments, no pools, 0↔1 autoscaling) but exercises the same API. `task dev:k8s` runs the full Kubernetes stack against a local kind cluster with live reload; see [development](development.md).
