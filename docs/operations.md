# Operations Guide

How to deploy and configure the orchestrator. Consumers of the API want the [jobs](jobs.md), [deployments](deployments.md), and [sandboxes](sandboxes.md) guides; [pools](pools.md) describe optional Revision capacity.

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

The Helm chart (`charts/orchestrator/`) installs up to six stateless components, plus the optional node mount agent DaemonSet:

Every component is opt-in — a default install renders nothing.

| Service | Enable with | Serves |
| --- | --- | --- |
| **jobs** | `jobs.enabled` | `/v1/jobs` — run-to-completion workloads (`batch/v1.Job` + a native sidecar per job) |
| **deployments** | `deployments.enabled` | `/v1/deployments` — long-lived HTTP workloads, optionally backed by warm capacity |
| **pool controller** | `poolController.enabled` | Bare warm-pod inventory for every enabled Revision and sandbox pool kind |
| **activator** | `deployments.activator.enabled` | Holds cold and async requests in front of deployments — required for scale-to-zero and `Prefer: respond-async` |
| **sandbox** | `sandbox.enabled` | `/v1/sandbox` — live workspaces at their own hostnames |
| **sandbox proxy** | `sandbox.proxy.enabled` | The data plane every sandbox request passes through, behind one wildcard route |

All derive their state from the cluster: restarts and replica failovers lose nothing, and there is no database.

## Prerequisites

- **Kubernetes 1.29+** — jobs rely on native sidecar containers. The chart declares this as `kubeVersion`, so an older cluster fails the install rather than accepting jobs that then never terminate. `helm template` checks the constraint against the Helm CLI's own built-in version — pass `--kube-version` if your CLI is older than your cluster.
- **Helm 3.8+** — the chart is published as an OCI artifact. Values are validated against `values.schema.json` on template/install/upgrade: unknown keys are rejected rather than silently ignored, so a misspelling fails loudly.
- For deployments: the **Gateway API CRDs** and a gateway controller with two *Extended* features — per-backendRef `RequestHeaderModifier` filters and `RegularExpression` header matching. Verified on **Traefik** (Gateway API v1.5+); Envoy Gateway and Istio also implement both. Without a gateway, set `deployments.httpRoute.enabled=false`: deployments still run, but you provide your own routing.

## Installing

```bash
helm install orchestrator oci://ghcr.io/open-runtimes/charts/orchestrator \
  --version <X.Y.Z> \
  --namespace orchestrator --create-namespace \
  --set jobs.enabled=true
```

This deploys the jobs service alone (components are opt-in; a bare install renders nothing). A complete worked example — serving plane, hardened workload namespace, pools, HA — lives at [`examples/helm-values.yaml`](../examples/helm-values.yaml). Verify with:

```bash
kubectl -n orchestrator port-forward svc/orchestrator-jobs 8080:8080 &
curl localhost:8080/readyz
```

To require authentication, put the key in a Secret and name it in `<plane>.auth.existingSecret` (key `<plane>.auth.secretKey`, default `api-key`); the chart mounts it and points `API_KEY_FILE` at it. Requests then need `Authorization: Bearer <key>`. **With no key configured, the API is open** — the services log a warning at startup.

## The values shape

Every plane (`jobs`, `deployments`, `sandbox`) and every component (`deployments.activator`, `sandbox.proxy`, `poolController`, `mountAgent`) takes the same keys for its own pods: `image`, `replicaCount`, `autoscaling`, `leaderElection`, `resources`, `podAnnotations`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `serviceAccount` and `service`. A plane adds what the pods it creates get: `namespace`, `networkPolicy`, `workload` (their `nodeSelector`, `tolerations` and overcommit), `httpRoute`, `auth` and `s3`. Shared images live under `images`, the Gateway under `gateway`. Resources are named `<fullname>-<component>`, where the fullname is the release name when it contains `orchestrator`, otherwise `<release>-orchestrator`.

## Exposing the APIs

Every plane's routes attach to one Gateway, `gateway.name` in `gateway.namespace` (by default the chart's fullname, in the release namespace). The chart can create it, or attach to one you manage:

```yaml
# values.yaml
gateway:
  create: true
  className: traefik
  listeners:
    - name: http
      port: 80
      protocol: HTTP
      allowedRoutes:
        namespaces: { from: All }   # required when a plane's namespace differs
jobs:
  enabled: true
  httpRoute:
    enabled: true
    hostnames:
      - jobs.example.com
```

The jobs API gets one route; the deployments service writes one route per deployment (`deployments.httpRoute.enabled`, on by default); sandboxes get one wildcard route to the sandbox proxy (`sandbox.httpRoute.enabled`).

## Enabling deployments

```yaml
# values.yaml
deployments:
  enabled: true
  activator:
    enabled: true # holds cold and async requests
  domain: apps.example.com        # auto-assigned hosts become {id}.apps.example.com
```

Point a wildcard DNS record (`*.apps.example.com`) at the gateway's address and deployments are reachable the moment they're ready.

## Pools

Pools are declared in values — adding, resizing, or removing one is a values change and rollout, not an API call:

```yaml
deployments:
  pools:
    - id: py
      image: python:3.12-slim
      port: 8000
      size: 4                # warm pods kept ready
      cpu: 1
      memory: 512
      burst: reject          # exhausted → direct fallback
    - id: node
      image: node:22-slim
      size: 2
      port: 3000             # >0 makes it an HTTP pool
      cpu: 1
      memory: 512
      burst: cold
```

Each pool keeps `size` pods warm at all times. Deployment users always submit image, port, CPU, memory, and the rest of their desired spec; the deployments service claims from an exact-shape match, while the generic `pool-controller` replenishes bare capacity off the request path. The same binary maintains sandbox pools under their separate pod contract. With no match or no accepted warm capacity the deployments service creates directly from the same Revision template. There is no deployment-pool API. See the [pools guide](pools.md).

## Sandboxes

[Sandboxes](sandboxes.md) are live workspaces reached at their own hostnames. Callers always submit a complete pod shape; `sandbox.pools` is optional operator-only capacity that is matched transparently. An exact match buys a sub-second claim, while no match or exhausted capacity creates the same pod directly. Operator config:

```yaml
sandbox:
  enabled: true
  domain: sandboxes.example.com   # needs a wildcard DNS record: *.sandboxes.example.com
  pools:
    - id: py
      image: python:3.12-slim    # any runtime image: the agent is installed into it
      port: 3000                 # where the agent listens; no command needed
      cpu: 1
      memory: 512
      size: 4
      runtimeClass: gvisor       # untrusted code is the expected workload here
  proxy:
    enabled: true                # the wildcard data plane every sandbox request passes through
```

`sandbox` sits alongside `jobs` and `deployments` because a sandbox is a workload kind, not a deployments feature: the API is served by its own sandbox-service, so it runs with or without the deployments plane. The chart fails the install when the proxy is enabled without the service, rather than rendering a proxy with nothing behind it.

Sandboxes also run on the [Docker development backend](sandboxes.md#the-docker-backend), without warm pools or isolation tiers. In production the sandbox proxy is its own Deployment behind one wildcard `HTTPRoute` for `*.{domain}` — not a mode of the activator: it is permanently on the request path, reads pods only, and raises nothing. Scale it for sandbox traffic (`sandbox.proxy.autoscaling`) rather than for cold starts.

**Mounting costs a privileged container, unless the node mount agent does it.** A loop mount needs `CAP_SYS_ADMIN` and root, so by default the sidecar performing it runs privileged and the shared workspace carries mount propagation. It is granted only where a mount was actually asked for:

| Workload | Privileged when |
| --- | --- |
| [Job](jobs.md#mount-artifact) | the request has a `mount` artifact — that pod only |
| [Deployment revision](deployments.md#the-request-spec) | the request has a `mount` artifact — every replica of that revision |
| [Sandbox](sandboxes.md#mounting-a-filesystem-image) / [pooled Revision](pools.md) | the **pool** sets `mounts: true` — every pod in the pool, since warm pods predate the claim |

None of them, with the node mount agent enabled (below). Without it, the last row is the one to watch: standing capacity means the privileged container is there before any request arrives, so treat a mounting pool as trusted infrastructure and keep untrusted workloads on pools without the capability.

**The node mount agent moves that privilege out of the pod** ([design](design/node-mount-agent.md)). With `mountAgent.enabled: true`, a DaemonSet serves each mounting pod's workspace as a CSI inline volume and performs its mounts from the node; the pod's sidecar stays unprivileged and asks the agent over HTTP with a token minted for that pod. Every replica of a revision and every warm pod gets a token of its own. Same artifact API, same mount layout, no measurable latency. The agent itself runs privileged on the host network, and its API is reachable on every node's IP: the per-pod token is the boundary, and only pods in the namespaces of the enabled planes (the job namespace with `jobs`, the workload namespace with `deployments` or `sandbox`) may use its driver, so treat the agent as node infrastructure. It must run on every node a mounting pod can land on, or that pod sits in `ContainerCreating`: by default it tolerates every workload toleration and runs on every node, so set `mountAgent.nodeSelector`/`tolerations` only to narrow that. Switching the agent on or off replaces mounting pools' warm pods, but a revision keeps the pod template it was minted with, so redeploy revisions that mount afterwards. Docker backends are unaffected.

**A sandbox URL is a credential.** Its hostname carries a 128-bit token, and reaching it is enough to run code inside the sandbox — so terminate TLS at the gateway (`sandbox.scheme: https`), and keep sandbox URLs out of access logs you would not treat as secrets.

## Configuration reference

The most consequential values (see `charts/orchestrator/values.yaml` for the full annotated set):

| Value | Default | Effect |
| --- | --- | --- |
| `<plane>.enabled` | `false` | Install a plane's service; every component is opt-in |
| `<plane>.namespace` | `""` | Namespace the plane's pods run in; empty is the release namespace (below) |
| `<plane>.networkPolicy.enabled` | jobs `false`, others `true` | Per-plane NetworkPolicy (below) |
| `<plane>.workload.{nodeSelector,tolerations}` | empty | Placement of the pods a plane creates (below) |
| `<plane>.workload.{cpu,memory}Overcommit` | `1` | Request = limit / overcommit for those pods (below) |
| `<plane>.replicaCount`, `.autoscaling`, `.leaderElection` | `1`, off, off | More than one replica, or an HPA, requires leader election |
| `<plane>.leaderElection.{leaseDurationSeconds,renewDeadlineSeconds,retryPeriodSeconds}` | `15` / `10` / `2` | Failure-detection bound and renewal budget; a hard leader loss delays work by roughly the lease duration |
| `<plane>.auth.existingSecret` | `""` | Bearer-token auth for the plane's API (above) |
| `<plane>.service.port` | `8080` | API port |
| `gateway.*` | not created | The Gateway every route attaches to (above) |
| `images.*` | GHCR, the chart's `appVersion` | Images placed in workload pods; `images.pullPolicy` is their pull policy |
| `jobs.workload.terminationGracePeriodSeconds` | `600` | How long a job pod's sidecar has for its post phase |
| `deployments.domain` | `localhost` | Base domain for auto-assigned hosts |
| `deployments.revisionWorkers` | `32` | Concurrent direct-Pod Revision reconciles |
| `deployments.clientQPS` / `deployments.clientBurst` | `200` / `400` | Client-side K8s API write budget for Revision and Pod bursts |
| `deployments.pools`, `sandbox.pools` | `[]` | Warm pool declarations (above) |
| `deployments.limitRange.enabled` | `false` | Default requests for unspecified containers |
| `sandbox.domain` | `""` | Wildcard domain sandboxes are addressed at (required) |
| `poolController.enabled` | `true` | Run warm-pod inventory for every enabled plane (also cleans removed pools) |
| `terminationGracePeriodSeconds` | `60` | Control-plane pod shutdown budget, including the final OTLP flush |
| `podSecurityContext`, `securityContext` | non-root, `RuntimeDefault`, read-only, no capabilities | Every control-plane pod and container; `<component>.podSecurityContext` / `.securityContext` replace them |
| `<plane>.workload.podSecurityContext` | `{}` (`RuntimeDefault` seccomp) | Pod security of every pod a plane creates (below) |
| `rbac.create` | `true` | Every Role, ClusterRole and binding the components need |
| `imagePullSecrets` | `[]` | Pull credentials for every pod the chart renders, plus the job-pod ServiceAccount (below) |
| `extraEnv` | `[]` | Extra environment for every control-plane container (e.g. `OTEL_EXPORTER_OTLP_ENDPOINT`, `AUTOSCALER_WINDOW`) |

Autoscaler tuning via `extraEnv`: `AUTOSCALER_WINDOW` (sliding window, default 60s) and `AUTOSCALER_TICK` (evaluation period, default 2s). A shorter window scales — in both directions — more aggressively.

## Private registries

`imagePullSecrets` supplies pull credentials to every pod the chart renders. Each entry is `{name: <secret>}`.

Workload pods — job pods, deployment replicas, warm pods — are created by the services rather than the chart, so pod-level values cannot reach them; they inherit credentials from the ServiceAccount they run as. The chart stamps `imagePullSecrets` onto the job-pod ServiceAccount it creates (`jobs.workload.serviceAccount`), which covers job pods. Deployment replicas, sandboxes and warm pods currently run under the `default` ServiceAccount of their plane's namespace, so a private *runtime* image there needs the secret attached to that account out of band.

A pull secret is referenced **by name only, and the name resolves in the namespace of the pod using it** — there is no cross-namespace reference. Every pod the chart renders lives in the release namespace, so that is where `imagePullSecrets` must exist. The job-pod ServiceAccount is the exception: the chart creates it in `jobs.namespace`. While that is the release namespace (the default) one secret covers everything, but once they differ the same name has to exist in the job namespace too, and `jobs.workload.serviceAccount.imagePullSecrets` overrides the list when the credentials are named differently there.

## Hardening

Defense in depth, each layer independently optional:

**Pod security floor (always on).** Every container the services run themselves (sidecars, init containers) runs as non-root (uid 65532) with all capabilities dropped and a read-only root filesystem, and no workload pod gets a ServiceAccount token — admissible under Pod Security Standards `restricted`. A user's own container keeps its image's user, without new privileges.

**Workload pod security** (`<plane>.workload.podSecurityContext`). The pod-level security context of every pod a plane's service creates, in the standard schema: seccomp and AppArmor profiles, SELinux options, `fsGroup`, supplemental groups, sysctls. Empty keeps `RuntimeDefault` seccomp; a profile you name replaces it, for every container in the pod, since containers set only the user, capabilities and privilege each needs. Changing it replaces warm pods.

```yaml
jobs:
  workload:
    podSecurityContext:
      seccompProfile:
        type: Localhost
        localhostProfile: profiles/builds.json   # under the kubelet's seccomp directory on every node
      appArmorProfile:
        type: RuntimeDefault
```

**A namespace per plane.** Each plane names the namespace its pods run in: `jobs.namespace`, `deployments.namespace` (revisions and their warm pools) and `sandbox.namespace` (sandboxes and theirs). Empty means the release namespace. Give each one its own, apart from the control plane. Sandboxes run untrusted code, so never put them in the same namespace as deployments. The chart creates what its components need in each namespace: the Roles and RoleBindings for that plane's control plane, a ServiceAccount per pool-controller kind that is allowed only in its own plane's namespace, and the job-pod ServiceAccount.

**The chart does not create or label namespaces.** That follows Helm's own practice: a chart that owns a namespace deletes everything in it when the release goes. Provision them where you manage cluster policy (ArgoCD `CreateNamespace` with `managedNamespaceMetadata`, Terraform, or `kubectl`), with their Pod Security level and any ResourceQuota:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: orchestrator-sandboxes
  labels:
    pod-security.kubernetes.io/enforce: restricted   # deployments and sandboxes: every pod the services create admits
---
apiVersion: v1
kind: Namespace
metadata:
  name: orchestrator-jobs
  labels:
    pod-security.kubernetes.io/enforce: baseline     # jobs: workers keep the image's user; use privileged for in-pod mounting without the node mount agent
```

**A NetworkPolicy per plane** (`<plane>.networkPolicy.enabled`), in the plane's namespace and selecting only its pods. Every policy blocks egress to the cloud metadata endpoint, the highest-value single rule against SSRF credential theft. Ingress depends on the plane:
- deployments (on by default): the gateway (`deployments.networkPolicy.gatewayNamespace`), the activator, the deployments service and the revision pool controller;
- sandboxes (on by default): the sandbox proxy, the sandbox service and the sandbox pool controller;
- jobs (off by default): none. Egress reaches the jobs service for artifact reports. What a build may reach is a platform decision, so enable this policy or ship your own. With the node mount agent, also allow the node on `mountAgent.port`, which NetworkPolicy cannot name (in Cilium: `toEntities: [host]`).

The policies need an enforcing CNI (Cilium, Calico; kindnet does not enforce).

<a id="isolation-tiers"></a>
**Isolation tiers.** Workloads can request stronger kernel isolation with `"runtimeClass": "gvisor"` or `"kata"` in their spec. Map tiers to your cluster's RuntimeClasses with `KUBE_RUNTIME_CLASSES` (e.g. `gvisor=gvisor,kata=kata-qemu` via `extraEnv`); the service validates the RuntimeClass exists before accepting the workload, so a missing runtime is a `400`, not a stuck pod.

**Secrets at rest.** Deployment specs — including callback signing keys — are stored in Secrets, not ConfigMaps or annotations. Pool claim tokens are HMAC-derived per pod from an install key that never leaves its Secret.

## Resource model

The client declares one ceiling per resource (`cpu` cores, `memory` MiB); the platform derives the scheduler request as `limit / overcommit`. The divisors are per-plane operator config — `<plane>.workload.{cpu,memory}Overcommit` covers the pods that plane creates (warm pools included) — and default to 1 (request = limit).

- **CPU**: request = `cpu / cpuOvercommit` and **no CPU limit** — CPU is compressible, and limits cause needless throttling. Raising `cpuOvercommit` packs workloads denser at the cost of contention under load.
- **Memory**: limit as declared, request = `memory / memoryOvercommit`. Memory is incompressible — overcommitting it trades OOM kills for density, so raise it only with headroom to spare.
- **Tolerations**: to run workloads on tainted node pools (e.g. `workload=edge-builds:NoSchedule`), set `<plane>.workload.tolerations` — a list in the standard pod-spec tolerations schema, stamped on every workload pod of that plane.
- **Node selector**: tolerations only *allow* the tainted pool; to *pin* pods to it, also set `<plane>.workload.nodeSelector` (e.g. `{workload: edge-builds}`) — a standard pod-spec nodeSelector stamped on every workload/job pod of that plane.
- Replicas spread across nodes via topology spread constraints, and durably multi-replica deployments get a PodDisruptionBudget automatically.
- `deployments.limitRange.enabled` adds defaults, in the deployments namespace, for containers that declare nothing, preventing BestEffort pods.

## Scaling the control plane

Set `deployments.replicaCount > 1` **and** `deployments.leaderElection.enabled=true`: all replicas serve the API (any replica can answer anything — state lives in the cluster), while rollout auto-cut, endpoint flip, and autoscaling run on the elected deployments-service leader. Pool inventory has a separate failure domain: set `poolController.replicaCount > 1` with `poolController.leaderElection.enabled=true`. Revision and sandbox inventory use distinct Leases and workloads, so either can fail without taking leadership away from consumer reconciliation or the other pool kind. The activator scales independently via `deployments.activator.replicaCount`. The chart fails fast when any multi-replica controller lacks leader election.

On Kubernetes, each domain revision is stored as an `orchestrator.open-runtimes.io/v1alpha1` `Revision`. The elected deployments-service controller creates its replica Pods directly using deterministic slots; Kubernetes `Deployment` and `ReplicaSet` controllers are not on the workload creation path. The `Revision` `/scale` subresource is shared by the autoscaler and cold-start activator.

**Upgrading from the `apps/v1` backend.** There is deliberately no migration: releases before the `Revision` CRD materialised each revision as a Kubernetes `Deployment`, and this backend neither adopts nor deletes those objects (its Role no longer holds any `apps` verbs). Delete existing deployments through the API before upgrading, or remove the leftover `apps/v1` Deployments and their ReplicaSets by hand afterwards; until then the old pods keep serving while the API reports the deployment with no revisions.

Each component can also autoscale on CPU (`<plane>.autoscaling.enabled`, `deployments.activator.autoscaling.enabled`, `sandbox.proxy.autoscaling.enabled` — min/max replicas and a target utilization); the services require leader election, the activator doesn't. Any component that is durably multi-replica (fixed count > 1, or an HPA) automatically gets a **PodDisruptionBudget** (`maxUnavailable: 1`) — never on singletons, where a PDB would block node drains.

The control-plane pods ship hardened by default: non-root with all capabilities dropped, `RuntimeDefault` seccomp, read-only root filesystems (`podSecurityContext` and `securityContext` at the top level, which a component's own replace), zero-downtime surge rollouts, soft topology spread across nodes, and the same no-CPU-limit resource shape as workloads (memory capped, CPU uncapped).

## Local development backend

Both services also run against Docker (`ORCHESTRATOR_BACKEND=docker`) for development: `docker compose up -d` at the repo root brings up the all-in-one `orchestrator` image — jobs, deployments and sandboxes in one process, on one API port (see [`docker-compose.yaml`](../docker-compose.yaml)), and `task dev` runs the jobs service from source with hot reload. The Docker backend is functionally reduced (single-revision deployments, no pools, 0↔1 autoscaling) but exercises the same API. `task dev:k8s` runs the full Kubernetes stack against a local kind cluster with live reload; see [development](development.md).
