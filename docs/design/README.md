# Serving Plane — Design

> Status: **shipped** (phases 0–6, see [implementation-plan.md](implementation-plan.md)). These are
> the internal design documents; consumers want the [user guides](../deployments.md) instead. The
> `cmd/deployments-service` binary runs long-lived / late-bound HTTP workloads alongside `/v1/jobs`,
> exposing **two APIs**: `/v1/deployments` (declarative, long-lived) and `/v1/deployment-pools`
> (pre-warmed, late-bound) — pools is a feature of the same service, not a separate binary. Backend
> selected via `ORCHESTRATOR_BACKEND=docker|kubernetes`. The jobs plane's internals are in
> [architecture.md](../architecture.md).

## The two APIs (one service)

- **[`/v1/deployments`](deployments-service.md)** — a container spec → a long-lived, HTTP-addressable
  workload with immutable revisions, traffic splitting, concurrency autoscaling, and scale-to-zero.
- **[`/v1/deployment-pools`](pools.md)** — **config-defined** warm fleets of an image; an activation
  late-binds a payload onto a warm pod (no cold start) and exposes it at a gateway URL. (Pools are
  declared in config; the API is read + activate.)

## We mirror Knative's data path — on standard primitives

We adopt Knative Serving's data-path architecture, **including the off-path optimization** (the
buffering edge leaves the request path when a revision is warm), but emit the **standard Kubernetes
Gateway API `HTTPRoute`** directly instead of Knative's `KIngress` CRD + `net-*` adapter.

Three on-path components, only one ours:

| Component | Role | On the path | Knative analogue |
|-----------|------|-------------|------------------|
| **[Gateway + HTTPRoute](gateway-routing.md)** | routing, weighted splitting, header match | always (warm) | Gateway (Kourier/Istio) |
| **[deployments-sidecar](deployments-sidecar.md)** (per-pod) | readiness, drain, metrics | always | queue-proxy |
| **[deployments-activator](deployments-activator.md)** (our Service) | buffer cold starts, own async | only when cold/async | Activator |

Plus the off-path control components: the **[deployments-autoscaler](deployments-autoscaler.md)**
(concurrency, scale-to-zero) and the **[orchestrator](orchestrator.md)** (materializes revisions/pods
per backend). Docker has no gateway, so it collapses the data plane into a single always-on in-process
proxy.

## Component docs

| Doc | Covers |
|-----|--------|
| [deployments-service.md](deployments-service.md) | the binary, both APIs, revisions/traffic/rollout, domain model |
| [pools.md](pools.md) | config-defined warm pools + activation (a feature of the service) |
| [gateway-routing.md](gateway-routing.md) | `HTTPRoute` adapter, off-path data path, traffic splitting, configurable URLs |
| [deployments-activator.md](deployments-activator.md) | cold-start buffering, async (`202` + callback), the SKS-equivalent cold endpoint flip |
| [deployments-sidecar.md](deployments-sidecar.md) | readiness gating, graceful drain, concurrency metrics, probes, shim mode |
| [deployments-autoscaler.md](deployments-autoscaler.md) | concurrency autoscaling, scale-to-zero, metric source |
| [orchestrator.md](orchestrator.md) | backend interface + Kubernetes / Docker backends |
| [resource-model.md](resource-model.md) | requests/limits/QoS, compaction, disruption & surge, scale limits |
| [security.md](security.md) | workload hardening, sandbox tiers, namespace model, network isolation |
| [failure-semantics.md](failure-semantics.md) | cross-cutting failure handling + status-code contract |
| [implementation-plan.md](implementation-plan.md) | phased build plan mapped onto the existing packages |

## Shared infrastructure (reused from the jobs service)

The binary imports `pkg/server`, the `internal/api` middleware chain, the `internal/artifact` pipeline
+ `job-sidecar` (reused as the `artifact-pre` container; the deployments-sidecar and pool shim are
dedicated binaries), `MemoryStore[T]` +
`LifecycleWatcher`, config + `ORCHESTRATOR_BACKEND` selection, and the **CloudEvents callback
dispatcher**.

**Callbacks.** The retry/circuit-breaking dispatcher delivers HMAC-signed CloudEvents. New types:
`orchestrator.deployment.revision.ready` / `.scaled` / `.deleted` (revision lifecycle),
`orchestrator.deployment.response` (async responses), and
`orchestrator.pool.activation.start` / `.log` / `.result`. The sync/async split lives at the edge
([deployments-activator](deployments-activator.md)) — the dispatcher and signing stay centralized,
never in workload pods.

**Statelessness.** No database; the backend (K8s objects / Docker labels) is the source of truth, and
in-memory state is rebuilt on `Start` via the `Reconcile()` pattern the Docker job backend already
uses.

## Key trade-offs

- **External gateway dependency** (prod: **Traefik**); Docker stays in-process. See [gateway-routing](gateway-routing.md).
- **Two on-path proxies** (gateway + deployments-sidecar) by design; the deployments-activator is
  on-path only when cold/async.
- **Net LoC is positive** — a serving plane can't be net-negative; the discipline is maximal reuse of
  jobs infra, standard Gateway API (no custom data plane), and concurrency-only autoscaling.
