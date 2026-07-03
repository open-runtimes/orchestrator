# Deployments Activator

The **buffering edge** — it sits on the request path **only when a revision is cold or a request is
async**; warm sync traffic bypasses it entirely (gateway → revision Service directly). It runs as an
ordinary Deployment + Service the gateway can target, stateless and horizontally scalable. On
**Docker** it is in-process and always on the path (no gateway).

The gateway tags every request it routes here with **`X-Revision`** (see
[gateway-routing](gateway-routing.md)), so the activator always knows the target revision — **it never
re-derives the traffic split.** Because `X-Revision` is trusted, the activator's **ingress is
gateway-only** (network policy) — a workload pod that could reach it directly could forge the header
and route to any revision (see [security](security.md)).

1. **Cold start** — a request to a scaled-to-zero revision is routed here (via the cold endpoint
   flip). The activator **buffers** it and **raises the revision `0→1` itself** — a patch of the
   revision Deployment's `scale` subresource, its only write RBAC (Docker: `Scale(_, 1)`) — never
   waiting for an autoscaler tick. It **watches the revision's pods directly** (informer) and **probes
   candidate pods' sidecars itself** (the Knative activator move): on the first successful direct
   probe it releases the buffered request **straight to that pod's IP:port** — never back through the
   routable Service (whose endpoints are the activator itself during the cold window, which would
   loop), and without waiting on kubelet readiness, EndpointSlice, or gateway propagation. Its queued
   count is scraped by the [deployments-autoscaler](deployments-autoscaler.md) to hold the revision up
   / scale past 1 while requests are buffered. If no pod becomes reachable within
   `responseStartTimeoutSeconds` (default 300s) it returns `503`; the buffer is bounded and overflow
   sheds with `503`.
2. **Async** (`Prefer: respond-async`) — the gateway header-matches async requests here (weighted, so
   the split is preserved). The activator returns **`202 Accepted`** + `X-Invocation-Id` immediately,
   forwards in the background to the `X-Revision` revision (its Service when warm, buffering when
   cold), and POSTs the response to the deployment's `callback` as `orchestrator.deployment.response`
   via the dispatcher (HMAC-signed, retried, circuit-broken). Requires a `callback` (else `400`).
   Async is **fire-and-forget**: the `X-Invocation-Id` is a correlation id only; no invocation is
   stored (stays stateless, no poll endpoint), and delivery is **at-most-once** — see
   [failure-semantics](failure-semantics.md). Response bodies above a configured cap (default 1 MiB)
   are truncated and flagged in the event.

The sync/async split lives entirely here — the [deployments-sidecar](deployments-sidecar.md) is
async-agnostic and never sees `Prefer`, the `202`, or the callback. This keeps `Prefer` parsing, HMAC
signing, and the dispatcher in one place rather than in every workload pod.

## Cold endpoint flip (the SKS mechanism)

The activator's presence is toggled at the **endpoint layer, not the route**: each revision's routable
Service is endpoint-managed (selectorless), and we reconcile its endpoint set by **ready-pod count,
not the autoscaler's intent**:

- **Warm** → endpoints are the revision's ready pods.
- **Zero ready pods (scale-to-zero, eviction, crash) or draining** → endpoints are the **activator**
  pods. The route's weighted backendRefs never change, so the split is preserved, and the
  per-backendRef `X-Revision` tag (see [gateway-routing](gateway-routing.md)) still tells the activator
  which revision it is.

**Best-effort timing, made correct by endpoint membership.** We add the activator to the endpoint set
before/while the last pod drains and remove it once a pod is ready — but `EndpointSlice` and
gateway-cache propagation are asynchronous, so a small race window remains. It is **non-fatal**
because the activator is a *member of the same endpoint set* throughout the cold/draining window (not
a separate Service we must fail over to): a stale read lands on the activator or a still-draining pod,
never on nothing, and forwarding via the activator is always safe. (This is the eventual consistency
Knative lives with, issue #14939; we make the race non-fatal, not instant.)

This is exactly Knative's ServerlessService public-Service mechanism (endpoints = pod IPs in serve
mode, activator IPs in proxy mode).

## Scale & resilience

The activator is shared across all deployments' cold/async traffic, so it's the one component that
sees that aggregate — run it as N stateless replicas (its own PDB at `minAvailable: 80%`, mirroring
Knative's component PDB). A lost activator replica drops the cold-start requests it was holding
(clients retry); warm sync traffic is unaffected because it never touched the activator.
