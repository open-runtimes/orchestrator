# Operations Guide

How to deploy and configure the orchestrator. Consumers of the API want the [jobs](jobs.md), [deployments](deployments.md), [pools](pools.md), and [sandboxes](sandboxes.md) guides instead.

- [What gets deployed](#what-gets-deployed)
- [Prerequisites](#prerequisites)
- [Installing](#installing)
- [Exposing the jobs API](#exposing-the-jobs-api)
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
| **activator** | `deployments.activator.enabled` | Holds cold and async requests in front of deployments — required for scale-to-zero and `Prefer: respond-async` |

Both derive all state from the cluster: restarts and replica failovers lose nothing, and there is no database.

## Prerequisites

- **Kubernetes 1.29+** — jobs rely on native sidecar containers. The chart declares this as `kubeVersion`, so an older cluster fails the install rather than accepting jobs that then never terminate. `helm template` checks the constraint against the Helm CLI's own built-in version — pass `--kube-version` if your CLI is older than your cluster.
- **Helm 3.8+** — the chart is published as an OCI artifact. Values are validated against `values.schema.json` on template/install/upgrade: unknown keys are rejected rather than silently ignored, so a misspelling fails loudly.
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

## Exposing the jobs API

The jobs `Service` is ClusterIP-only by default — the chart renders no Ingress or Gateway API resources unless asked. To expose it via Gateway API:

```yaml
# values.yaml
jobs:
  enabled: true
  gateway:
    enabled: true
    gatewayClassName: traefik
    listeners:
      - name: web
        port: 8000
        protocol: HTTP
        allowedRoutes:
          namespaces: { from: Same }
  httpRoute:
    enabled: true
    hostnames:
      - jobs.example.com
```

Unlike `deployments.gateway` (a pointer the deployments-service's own reconciler uses against a Gateway you already own), the jobs API has no reconciler — the chart renders both the `Gateway` and `HTTPRoute` directly. Leave `jobs.gateway.enabled: false` and set `jobs.httpRoute.parentRefs` instead if you'd rather attach to a Gateway you manage elsewhere (e.g. one shared across several releases).

## Enabling deployments

```yaml
# values.yaml
deployments:
  enabled: true
  activator:
    enabled: true # holds cold and async requests
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

## Sandboxes

[Sandboxes](sandboxes.md) are live workspaces created from their own warm pools, reached at their own hostnames. Three pieces of operator config:

```yaml
sandboxes:
  domain: sandboxes.example.com   # needs a wildcard DNS record: *.sandboxes.example.com
  pools:
    - id: py
      image: python:3.12-slim    # any runtime image: the agent is installed into it
      port: 3000                 # where the agent listens; no command needed
      size: 4
      runtimeClass: gvisor       # untrusted code is the expected workload here
      maxIdleSeconds: 900        # ceiling on how long one may hold a warm pod
  proxy:
    enabled: true                # the wildcard data plane every sandbox request passes through
```

`sandboxes` sits alongside `jobs` and `deployments` because a sandbox is a workload kind, not a deployments feature — but the API is served by the deployments service today, so `deployments.enabled` must be true as well. The chart fails the install if it is not, rather than rendering a proxy with nothing behind it.

Sandboxes also run on the [Docker development backend](sandboxes.md#the-docker-backend), without warm pools or isolation tiers. In production the sandbox proxy is its own Deployment behind one wildcard `HTTPRoute` for `*.{domain}` — not a mode of the activator: it is permanently on the request path, reads pods only, and raises nothing. Scale it for sandbox traffic (`sandboxes.proxy.autoscaling`) rather than for cold starts.

**Mounting costs a privileged container, wherever it happens.** A loop mount needs `CAP_SYS_ADMIN` and root, so the sidecar performing it runs privileged and the shared workspace carries mount propagation. It is granted only where a mount was actually asked for:

| Workload | Privileged when |
| --- | --- |
| [Job](jobs.md#mount-artifact) | the request has a `mount` artifact — that pod only |
| [Deployment revision](deployments.md#the-request-spec) | the request has a `mount` artifact — every replica of that revision |
| [Sandbox](sandboxes.md#mounting-a-filesystem-image) / [activation](pools.md#artifacts) | the **pool** sets `mounts: true` — every pod in the pool, since warm pods predate the claim |

The last row is the one to watch: standing capacity means the privileged container is there before any request arrives, so treat a mounting pool as trusted infrastructure and keep untrusted workloads on pools without the capability.

**A sandbox URL is a credential.** Its hostname carries a 128-bit token, and reaching it is enough to run code inside the sandbox — so terminate TLS at the gateway (`sandboxes.scheme: https`), and keep sandbox URLs out of access logs you would not treat as secrets.

## Configuration reference

The most consequential values (see `charts/orchestrator/values.yaml` for the full annotated set):

| Value | Default | Effect |
| --- | --- | --- |
| `jobs.image`, `jobs.sidecarImage` | GHCR latest | Jobs service and per-job sidecar images |
| `jobs.gateway.{enabled,gatewayClassName,listeners}` | disabled | Renders a `Gateway` for the jobs API (above) |
| `jobs.httpRoute.{enabled,parentRefs,hostnames}` | disabled | Renders an `HTTPRoute` for the jobs API (above) |
| `deployments.enabled` | `false` | Install the deployments service + activator |
| `deployments.domain` | `localhost` | Base domain for auto-assigned hosts |
| `deployments.gateway.{enabled,name,namespace}` | `true`, `orchestrator`, release ns | The Gateway that HTTPRoutes attach to |
| `deployments.dataPort` | `8081` | Docker-backend data plane / activator data port |
| `deployments.pools` | `[]` | Warm pool declarations (above) |
| `deployments.workloadNamespace.*` | disabled | Hardened namespace for workload pods (below) |
| `deployments.{cpu,memory}Overcommit` | `1` | Request = limit / overcommit for workloads (below) |
| `jobs.{cpu,memory}Overcommit` | `1` | Same, independently for job pods |
| `deployments.workloadTolerations` | `[]` | Tolerations on workload pods (deployment replicas + warm pools) |
| `jobs.workloadTolerations` | `[]` | Tolerations on job pods |
| `deployments.workloadNodeSelector` | `{}` | Node selector pinning workload pods to a node pool |
| `jobs.workloadNodeSelector` | `{}` | Same, independently for job pods |
| `deployments.leaderElection.enabled` | `false` | Required when `deployments.replicaCount > 1` |
| `deployments.limitRange.enabled` | `false` | Default requests for unspecified containers |
| `deployments.activator.replicaCount` | `1` | Activator replicas (deployment mode) |
| `service.apiPort` / `service.metricsPort` | `8080` / `9090` | API and Prometheus ports |
| `imagePullSecrets` | `[]` | Pull credentials for every pod the chart renders, plus the job-pod ServiceAccount (below) |
| `extraEnv` | `[]` | Extra environment for the services (e.g. `AUTOSCALER_WINDOW`, `API_KEY_FILE`) |

Autoscaler tuning via `extraEnv`: `AUTOSCALER_WINDOW` (sliding window, default 60s) and `AUTOSCALER_TICK` (evaluation period, default 2s). A shorter window scales — in both directions — more aggressively.

## Private registries

`imagePullSecrets` supplies pull credentials to every pod the chart renders. Each entry is `{name: <secret>}`.

Workload pods — job pods, deployment replicas, warm pods — are created by the services rather than the chart, so pod-level values cannot reach them; they inherit credentials from the ServiceAccount they run as. The chart stamps `imagePullSecrets` onto the job-pod ServiceAccount it creates (`serviceAccount.jobSidecarCreate`), which covers job pods. Deployment replicas and warm pods currently run under the workload namespace's `default` ServiceAccount, so a private *runtime* image there needs the secret attached to that account out of band.

A pull secret is referenced **by name only, and the name resolves in the namespace of the pod using it** — there is no cross-namespace reference. Every pod the chart renders lives in the release namespace, so that is where `imagePullSecrets` must exist. The job-pod ServiceAccount is the exception: the chart creates it in `orchestrator.jobNamespace`. While that is the release namespace (the default) one secret covers everything, but once they differ the same name has to exist in the job namespace too, and `serviceAccount.jobSidecarImagePullSecrets` overrides the list when the credentials are named differently there.

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

<a id="isolation-tiers"></a>
**Isolation tiers.** Workloads can request stronger kernel isolation with `"runtimeClass": "gvisor"` or `"kata"` in their spec. Map tiers to your cluster's RuntimeClasses with `KUBE_RUNTIME_CLASSES` (e.g. `gvisor=gvisor,kata=kata-qemu` via `extraEnv`); the service validates the RuntimeClass exists before accepting the workload, so a missing runtime is a `400`, not a stuck pod.

**Secrets at rest.** Deployment specs — including callback signing keys — are stored in Secrets, not ConfigMaps or annotations. Pool claim tokens are HMAC-derived per pod from an install key that never leaves its Secret.

## Resource model

The client declares one ceiling per resource (`cpu` cores, `memory` MiB); the platform derives the scheduler request as `limit / overcommit`. The divisors are per-plane operator config — `deployments.{cpu,memory}Overcommit` covers deployment replicas and warm-pool pods, `jobs.{cpu,memory}Overcommit` covers job pods — and default to 1 (request = limit).

- **CPU**: request = `cpu / cpuOvercommit` and **no CPU limit** — CPU is compressible, and limits cause needless throttling. Raising `cpuOvercommit` packs workloads denser at the cost of contention under load.
- **Memory**: limit as declared, request = `memory / memoryOvercommit`. Memory is incompressible — overcommitting it trades OOM kills for density, so raise it only with headroom to spare.
- **Tolerations**: to run workloads on tainted node pools (e.g. `workload=edge-builds:NoSchedule`), set `deployments.workloadTolerations` / `jobs.workloadTolerations` — a list in the standard pod-spec tolerations schema, stamped on every workload/job pod of that plane.
- **Node selector**: tolerations only *allow* the tainted pool; to *pin* pods to it, also set `deployments.workloadNodeSelector` / `jobs.workloadNodeSelector` (e.g. `{workload: edge-builds}`) — a standard pod-spec nodeSelector stamped on every workload/job pod of that plane.
- Replicas spread across nodes via topology spread constraints, and durably multi-replica deployments get a PodDisruptionBudget automatically.
- `limitRange.enabled` adds defaults for containers that declare nothing, preventing BestEffort pods.

## Scaling the control plane

Set `deployments.replicaCount > 1` **and** `deployments.leaderElection.enabled=true`: all replicas serve the API (any replica can answer anything — state lives in the cluster), while the background reconcilers (rollout auto-cut, endpoint flip, autoscaler, pool control) run on the elected leader only. The activator scales independently via `deployments.activator.replicaCount`. The chart **fails fast at render time** if you ask for multiple replicas without leader election.

Each component can also autoscale on CPU (`autoscaling.enabled`, `deployments.autoscaling.enabled`, `deployments.activator.autoscaling.enabled` — min/max replicas and a target utilization); the services require leader election, the activator doesn't. Any component that is durably multi-replica (fixed count > 1, or an HPA) automatically gets a **PodDisruptionBudget** (`maxUnavailable: 1`) — never on singletons, where a PDB would block node drains.

The control-plane pods ship hardened by default: non-root with all capabilities dropped, `RuntimeDefault` seccomp, read-only root filesystems, zero-downtime surge rollouts, soft topology spread across nodes, and the same no-CPU-limit resource shape as workloads (memory capped, CPU uncapped).

## Local development backend

Both services also run against Docker (`ORCHESTRATOR_BACKEND=docker`) for development: `docker compose up -d` at the repo root brings up both against your local daemon (see [`docker-compose.yaml`](../docker-compose.yaml)), and `task dev` runs the jobs service from source with hot reload. The Docker backend is functionally reduced (single-revision deployments, no pools, 0↔1 autoscaling) but exercises the same API. `task dev:k8s` runs the full Kubernetes stack against a local kind cluster with live reload; see [development](development.md).
