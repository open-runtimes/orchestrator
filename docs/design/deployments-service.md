# Deployments Service

The `cmd/deployments-service` binary. Turns a container spec into a long-lived, HTTP-addressable
workload, and **also hosts the [pools](pools.md) API** (`/v1/deployment-pools`) — pools is a feature of this
service, not a separate binary. Backend selected via `ORCHESTRATOR_BACKEND=docker|kubernetes`;
everything above the backend is backend-agnostic.

See [README](README.md) for the shared architecture and component map.

## Object model

The one Knative split we keep — delivers rollback and canary:

```
Deployment (id)                          ← stable identity + HTTPRoute
 ├─ Revision id-00001 (immutable spec)   ← minted on each spec change; retained for rollback
 ├─ Revision id-00002
 └─ traffic: [{rev: id-00002, %: 90}, {rev: id-00001, %: 10}]
```

- A **Revision** is an immutable snapshot of the container spec — a `POST` whose spec differs from the
  head mints a new one. Each revision is an `apps/v1.Deployment` + a ClusterIP Service (see
  [orchestrator](orchestrator.md)).
- **Default rollout — auto-cut to latest.** Minting a revision shifts 100% of traffic to it **once it
  reports ready** (latest-ready); the previous revision drains, then is eligible for GC.
  `POST /traffic` overrides to pin or split weights (canary/blue-green) or roll back.
- **Retention.** The most recent `revisionHistoryLimit` (default 3) revisions plus any still
  receiving traffic are kept; older are `Retire`d — each retained revision is ~3 etcd objects, and the
  default directly sets the registered-deployments ceiling (see
  [resource-model](resource-model.md#scale--limits)).
- The **traffic table** compiles to weighted `backendRefs` on the deployment's
  [`HTTPRoute`](gateway-routing.md).

## HTTP API

```
POST   /v1/deployments                 # create-or-update spec → mints a Revision (K8s)
GET    /v1/deployments                 # list
GET    /v1/deployments/{id}            # status: revisions, traffic, replicas, url
GET    /v1/deployments/{id}/revisions  # revision history
POST   /v1/deployments/{id}/traffic    # shift % across revisions — canary/blue-green/rollback (K8s)
DELETE /v1/deployments/{id}

# deployment-pools (same binary) — pools are config-defined; API is read + activate (no create/delete) — see pools.md
GET    /v1/deployment-pools  /  GET /v1/deployment-pools/{id}
POST   /v1/deployment-pools/{id}/activate         GET/DELETE /v1/deployment-pools/{id}/activations/{actId}
```

There is no `/proxy/*` route — the **gateway URL is the endpoint** (the in-process activator's URL on
Docker), in `StatusResponse.URL`. Async is per-request via `Prefer: respond-async`, routed to the
[deployments-activator](deployments-activator.md). The management middleware chain (recovery → logging
→ metrics → CORS → content-type → auth) is reused verbatim from `internal/api`.

## Domain model

```go
// pkg/deployment/types.go
type Request struct {
    ID          string              `json:"id"`                    // RFC-1123 label (≤63); part of k8s object names
    Meta        map[string]string   `json:"meta"`
    Image       string              `json:"image"`
    Command     string              `json:"command,omitempty"`
    CPU         float64             `json:"cpu"`                   // limit (cores); request derived via overcommit ratio
    Memory      int                 `json:"memory"`                // limit (MB); request ≈ limit (OOM-safe)
    Sandbox     string              `json:"sandbox,omitempty"`     // RuntimeClass: runc (default) | gvisor | kata
    Environment map[string]string   `json:"environment"`
    Artifacts   []artifact.Artifact `json:"artifacts,omitempty"`   // materialized into the workspace before serving
    Host        string              `json:"host,omitempty"`        // RFC-1123 hostname (≤253); else {id}.{deployments-domain}
    Port        int                 `json:"port"`                  // container port serving HTTP
    Replicas    int                 `json:"replicas"`              // fixed count when autoscaling is unset; default 1
    Concurrency int                 `json:"concurrency,omitempty"` // hard per-pod cap (k8s containerConcurrency); 0 = unlimited
    Autoscaling *Autoscaling        `json:"autoscaling,omitempty"` // K8s; when set, manages replicas
    Probes      *Probes             `json:"probes,omitempty"`
    Callback    *Callback           `json:"callback,omitempty"`
    TimeoutSeconds              int `json:"timeoutSeconds,omitempty"`              // per-request total → 504; default 300
    ResponseStartTimeoutSeconds int `json:"responseStartTimeoutSeconds,omitempty"` // activator wait for a ready endpoint → 503; default 300
    ProgressDeadlineSeconds     int `json:"progressDeadlineSeconds,omitempty"`     // revision-ready deadline → failed; default 600
}

// Probes — only Readiness is sidecar-run (honors ms granularity); Liveness/Startup are kubelet-run
// at whole-second granularity (ms rounded up, 1s minimum). See deployments-sidecar.md.
type Probes struct {
    Readiness *Probe `json:"readiness,omitempty"` // sidecar-run; gates traffic / endpoint membership; sub-second
    Liveness  *Probe `json:"liveness,omitempty"`  // kubelet-run; restarts the container; ≥1s
    Startup   *Probe `json:"startup,omitempty"`   // kubelet-run; slow-boot grace; ≥1s
}

// Probe mirrors k8s Probe. PeriodMillis/TimeoutMillis are honored sub-second ONLY for the sidecar-run
// readiness probe; for kubelet-run liveness/startup they round up to whole seconds (1s min).
type Probe struct {
    Path             string `json:"path,omitempty"`             // HTTP GET path on Port; empty = TCP connect
    PeriodMillis     int    `json:"periodMillis,omitempty"`     // k8s periodSeconds
    TimeoutMillis    int    `json:"timeoutMillis,omitempty"`    // k8s timeoutSeconds
    FailureThreshold int    `json:"failureThreshold,omitempty"` // k8s failureThreshold; give-up = threshold × period
}

// Autoscaling mirrors HPA naming. minReplicas: 0 enables scale-to-zero.
type Autoscaling struct {
    MinReplicas int `json:"minReplicas"` // 0 = scale-to-zero
    MaxReplicas int `json:"maxReplicas"`
    Target      int `json:"target"`      // target in-flight concurrency per replica
}

type Target struct {
    RevisionName string `json:"revisionName"`
    Percent      int    `json:"percent"`
}

type StatusResponse struct {
    ID                string   `json:"id"`
    State             string   `json:"status"` // pending|ready|degraded|failed|deleting
    URL               string   `json:"url"`    // gateway URL (K8s) / activator URL (Docker)
    Revisions         []string `json:"revisions"`
    Traffic           []Target `json:"traffic"`
    DesiredReplicas   int      `json:"desiredReplicas"`
    AvailableReplicas int      `json:"availableReplicas"`
    Error             string   `json:"error,omitempty"`
}
```

`cpu`/`memory` are **limits**; the platform derives requests (see [resource-model](resource-model.md)).
Artifacts run before serving (pre-artifacts via a run-once `artifact-pre` init container; post-artifacts
fold into the sidecar's drain). `host` and configurable URLs: see [gateway-routing](gateway-routing.md).

## Statelessness

No database — the backend is the source of truth (revisions = immutable Deployments + Services; the
`HTTPRoute` holds the traffic table, and each revision Service's `EndpointSlice` holds the cold flip
(ready pods ↔ activator); scaling bounds, shard assignment, and last-ready revision live on the
per-deployment **marker ConfigMap** — the one object that exists for a deployment in every phase).
On `Start`, informers rebuild in-memory state via the `Reconcile()` pattern. Any replica
serves status; the [activator](deployments-activator.md) and
[autoscaler](deployments-autoscaler.md) are leader-elected / horizontally scalable.

## Delivery phases

The gateway/off-path arrives in Phase 3; before it the in-process activator handles all routing.

| Phase | Delivers | Backends |
|-------|----------|----------|
| **1** | stable proxied URL via in-process activator, deployments-sidecar readiness + drain, sync + async, fixed replicas | Docker + K8s |
| **2** | scale-to-zero (`0↔1`) | Docker + K8s |
| **3** | Gateway API `HTTPRoute`: off-path routing + traffic splitting + immutable revisions/rollback | K8s |
| **4** | concurrency autoscaling (`1↔N`) from sidecar metrics | K8s |
| **5** | pools (warm fleets + activation) — see [pools](pools.md) | Docker + K8s |
