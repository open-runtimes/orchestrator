# Pools

A feature of the [deployments-service](deployments-service.md) (same binary, `/v1/deployment-pools`), not a
separate service. A **warm pool** keeps a configured number of generic pods of a base image running
and idle; an **activation** late-binds a workload onto one, so it pays **no Kubernetes cold start** —
*claim + inject payload + exec* instead of *schedule + pull + start + readiness*. This is the
open-runtimes execution model: a pool is a warm fleet of a runtime image.

## Concept

- A **Pool** is declared in **service configuration**, not via the API: `image`, target `size`,
  `cpu`/`memory`/`sandbox`, serving `port`/`probes`. A leader-elected replenishment controller warms
  `size` pods from config on startup. A pool is **standing warm capacity** (idle cost), so it's an
  operator/config decision — adding, resizing, or removing a pool is a config change + rollout, not a
  runtime call.
- A warm **pod** = a **main container** (pool image, entrypoint overridden to the dedicated
  **pool-shim** binary, idle on a FIFO) + the
  [**deployments-sidecar**](deployments-sidecar.md), sharing an `emptyDir`. The sidecar is the pod's
  HTTP front, **listening from pod start** — so *activation is just an HTTP POST to the claimed pod's sidecar*, no out-of-band channel.
- An **Activation** (`POST /v1/deployment-pools/{id}/activations`) ships **artifacts** + a command. The sidecar
  materializes artifacts, signals the shim to `exec` the entrypoint, gates readiness, and the adapter
  exposes the pod at a **gateway URL** ([per-activation Service + `HTTPRoute`](gateway-routing.md)).
- **Two modes:** **run-to-completion (exec)** — the command exits with an exit code + output;
  **sync by default**, the `activate` call blocks and returns them inline (bounded by `timeoutSeconds`),
  or with `Prefer: respond-async` returns `202` and delivers them via the `.result` callback — which is
  then **required** (`400` if absent). So `callback` is optional precisely because sync returns inline;
  nothing is stored or polled (stateless preserved). Exec activations create **no Service/`HTTPRoute`**
  — results flow back over the claim connection or the callback, so exec latency is pure
  claim+inject+exec. **Long-lived HTTP** — serves at the gateway URL until idle timeout / `DELETE`.
  `ExitCode`/`Output` apply only to exec (`Output` capped, default 1 MiB, truncated + flagged); the URL
  only to HTTP — and its availability is bounded by **route programming/propagation latency**
  (Service + `HTTPRoute` + gateway config), which can rival a container start on some controllers:
  measure it before quoting warm-pool numbers for HTTP mode.
- **One activation per pod**, never reused; discarded at end (exit / idle / `DELETE`) and the slot
  replenished off the request path → **no cross-activation leakage**.

## HTTP API

Pools are config-defined (above), so the API is **read + activate** only — no create/delete:

```
GET    /v1/deployment-pools                            # list configured pools + warm/claimed counts
GET    /v1/deployment-pools/{id}                       # pool status
POST   /v1/deployment-pools/{id}/activations              # claim + bind → { id, url, ... }
GET    /v1/deployment-pools/{id}/activations           GET/DELETE /v1/deployment-pools/{id}/activations/{actId}
```

## Domain model

```go
// pkg/pool/types.go — Pool is the service-config schema (loaded at startup, e.g. a `pools:` list
// in Helm values); Activation / ActivationStatus are the runtime API types.
type Pool struct {
    ID, Image   string
    Size        int               `json:"size"`                  // warm pods kept ready
    CPU         float64           `json:"cpu"`
    Memory      int               `json:"memory"`
    Sandbox     string            `json:"sandbox,omitempty"`     // RuntimeClass; warm fleets keyed by (image, sandbox)
    Port        int               `json:"port"`                  // HTTP port the runtime serves on
    Probes      *Probes           `json:"probes,omitempty"`      // same types as deployments
    Environment map[string]string `json:"environment,omitempty"`
    Meta        map[string]string `json:"meta,omitempty"`
}

type Activation struct {
    ID                 string              `json:"id,omitempty"`          // caller-chosen (stable URL), RFC-1123 label; else generated
    Host               string              `json:"host,omitempty"`        // RFC-1123 hostname; else {id}.{pool-domain}
    Command            string              `json:"command"`
    Environment        map[string]string   `json:"environment,omitempty"`
    Artifacts          []artifact.Artifact `json:"artifacts,omitempty"`   // job artifact types
    TimeoutSeconds     int                 `json:"timeoutSeconds,omitempty"`
    IdleTimeoutSeconds int                 `json:"idleTimeoutSeconds,omitempty"` // tear down after idleness; 0 = until DELETE
    Callback           *Callback           `json:"callback,omitempty"`
}

type ActivationStatus struct {
    ID, PoolID, PodID string
    URL               string `json:"url"`           // gateway URL for ongoing HTTP
    State             string `json:"status"`        // activating|ready|exited|failed|deactivating
    ExitCode          *int   `json:"exitCode,omitempty"` // exec only
    Output            string `json:"output,omitempty"`
    Error             string `json:"error,omitempty"`
}
```

## Claim & replenishment

**The sidecar POST *is* the claim.** The service lists free warm pods (K8s: `pool.state=warm` via the
informer; Docker: in-memory free-list), picks one, and POSTs the activation to its sidecar. The
sidecar accepts the first and returns **`409`** to any racing replica, which retries the next free pod
— the pod is the serialization point, so the service stays stateless.

**The claim is authenticated.** The service injects a random per-pod **claim token** into the warm pod
at creation; the sidecar rejects an activation POST without it (`401`). Warm-pod ingress is further
restricted to the deployments-service by network policy (see [security](security.md)) — the sidecar
listening from pod start must not mean in-cluster callers can inject workloads past API auth.

**Claim sequence & crash recovery.** Accept (sidecar) → label the pod `pool.activation=<id>` → create
the per-activation Service/`HTTPRoute` (HTTP mode only). A service crash between steps leaves a
*claimed-but-unlabeled* pod; reconcile discards any pod whose sidecar reports claimed (its status
endpoint) but that carries no activation label after a short TTL, and replenishment replaces it —
orphans are garbage, never resold.

**Replenishment** is a leader-elected controller: it reconciles `warm_count → size` (a pod counts once
its sidecar is **warm-ready** — accepting activations — distinct from the post-activation
**serving-ready** gate), creating replacements **off the request path** as pods are claimed/terminate.

**Burst beyond `size`** falls back to cold-create or `429` (configurable, always logged).

## Cross-references

- **Gateway URL / configurable host:** [gateway-routing](gateway-routing.md). Async (`202` + callback)
  lives in the [deployments-activator](deployments-activator.md), not the sidecar.
- **Sandbox is a `Pool` dimension** (warm pods are runtime-fixed at creation): [security](security.md).
- **Failures** (activation-never-ready, exec exit, claim race, burst): [failure-semantics](failure-semantics.md#pools-specific).
- **Statelessness:** an active instance is backend state (claimed pod labeled `pool.activation=<id>`,
  its per-activation Service + `HTTPRoute`); reconstructed on `Start` by listing these.
