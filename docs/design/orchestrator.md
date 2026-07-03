# Orchestrator (backends)

The backend-agnostic interface that materializes revisions as running, routable pods. Selected at
startup via `ORCHESTRATOR_BACKEND=docker|kubernetes`. Mirrors `pkg/job/orchestrator.go` with two
changes: `Run`→`Apply` (declarative create-or-update) and a new `Endpoints` for the deployments-activator.

```go
// pkg/deployment/orchestrator.go
type Orchestrator interface {
    Start(ctx context.Context) error
    Apply(ctx context.Context, rev *Revision) error                  // materialize immutable revision (Deployment + Service)
    SetTraffic(ctx context.Context, id string, t []Target) error     // reconcile HTTPRoute weights (K8s)
    Scale(ctx context.Context, revID string, replicas int) error     // 0 = scale-to-zero (both); 1..N = K8s only
    Endpoints(ctx context.Context, revID string) ([]*url.URL, error) // ready WORKLOAD pod endpoints — the activator forwards here directly (never the routable Service, which is the activator while cold)
    Status(ctx context.Context, id string) (*StatusResponse, error)
    List(ctx context.Context) ([]StatusResponse, error)
    Retire(ctx context.Context, revID string) error                  // GC old revisions
    Ready(ctx context.Context) error
    Close() error
}
```

`SetTraffic` reconciles the deployment's [`HTTPRoute`](gateway-routing.md) weights; the **cold endpoint
flip** reconciles the routable Service's `EndpointSlice` (ready pods ↔ activator pods). `Endpoints`
returns the revision's **ready pod IPs**, so the [deployments-activator](deployments-activator.md)
releases a buffered request **straight to a pod** — *not* via the routable Service, whose endpoints are
the activator itself during the cold window (which would loop).

## Kubernetes backend

- One `apps/v1.Deployment` **per revision**; the [deployments-autoscaler](deployments-autoscaler.md) patches `spec.replicas`
  within `[minReplicas, maxReplicas]` (0 allowed) via the `scale` subresource (the
  [deployments-activator](deployments-activator.md) raises `0→1` through the same subresource — its
  only write).
- One **routable (selectorless, endpoint-managed) Service per revision** — the gateway's backendRef
  target. We reconcile its `EndpointSlice`: **ready workload pods** when warm, **activator** pods when
  cold/draining (the SKS-style cold flip). **Port contract:** the Service port is stable and the
  reconciled `EndpointSlice` sets the target port per endpoint set — the workload's `Port` for pod
  endpoints, the activator's listen port for activator endpoints — so the flip is always routable.
  (Envoy-class controllers load-balance to these endpoints directly, bypassing kube-proxy, as a
  scaling optimization; correctness only needs endpoint readiness — see [gateway-routing](gateway-routing.md).)
- One **`HTTPRoute` per deployment** — fixed weighted backendRefs (each with a per-backendRef
  `X-Revision` header, Gateway API Extended) + a `Prefer: respond-async` rule.
- The **deployments-activator** runs as an ordinary Deployment + Service the gateway can target.
- Pod template, sharing an `emptyDir` workspace: `artifact-pre` **init container** (`job-sidecar` in
  pre mode) materializes artifacts before the server starts; the **deployments-sidecar** native sidecar
  (`-mode=proxy`); the user **server** container. All under the hardened SecurityContext +
  `RuntimeClass` (see [security](security.md)).
- Readiness via deployments-sidecar; drain via its `preStop`. `Endpoints` returns the revision's
  **ready Pod IPs** (the activator's direct forward target); `Status` derives from
  `Deployment.Status.AvailableReplicas`.
- **Failure:** a revision that never passes readiness holds `pending`/`degraded`; past
  `progressDeadlineSeconds` (the backing Deployment's `spec.progressDeadlineSeconds`) → `failed`, and a
  failed *new* revision doesn't get the auto-cut traffic shift. See [failure-semantics](failure-semantics.md).

Labels: `managed-by=deployments-service`, `deployment.id`, `deployment.revision`.

## Docker backend (dev convenience)

- No gateway, no off-path: an in-process **deployments-activator proxy** is always on the path
  (`deployments-activator → deployments-sidecar → user container`).
- Per deployment on the shared `DOCKER_NETWORK`: a **deployments-sidecar container** fronting the user
  container, preceded by an artifact step.
- `Apply` recreates the container on spec change; `SetTraffic` is a no-op (single revision).
- `Scale` supports `{0,1}`: `Scale(_, 0)` stops (idle-to-zero), `Scale(_, 1)` starts; the deployments-activator
  buffers and triggers `Scale(_, 1)` on a cold hit — so scale-to-zero matches K8s.
- `Endpoints` returns the deployments-sidecar container IP.

Pools reuse the same backends with a [warm-pod](pools.md) template (init shim-install +
deployments-sidecar + the pool image entrypoint-overridden to the shim).
