# Gateway & Routing

The data plane for warm traffic. Our service is the **control-plane adapter** — it reconciles a
standard Gateway API **`HTTPRoute`** per deployment (or per pool activation). The data plane is a
Gateway API controller — basic serving works on any conformant one, but the canary-preserving cold/
async path needs **Extended** backendRef-filter support (see Revision identity below): prod is
**Traefik**; Envoy Gateway / Istio also qualify. No bespoke proxy, no `KIngress` CRD.

## Off-path warm data path

```
client → gateway → revision Service → deployments-sidecar → user container
```

Warm traffic never touches our service. **Basic serving (routing + readiness) rests only on standard
`HTTPRoute` → `Service` semantics** and works on any conformant controller: a pod is in the routable
Service's endpoints only when its [deployments-sidecar](deployments-sidecar.md) reports ready (we
manage that endpoint-managed Service, SKS-style), so readiness gating holds (kube-proxy, too, only
load-balances to ready endpoints). The Extended dependency below applies only to the canary
cold/async path, not to plain serving.

As a **performance** detail, Envoy-class controllers (including **Traefik**) resolve the Service's
`EndpointSlice`s and load-balance to pod IPs directly, bypassing kube-proxy — which matters at scale
(see [resource-model](resource-model.md)). That is controller behavior, not something standard
`HTTPRoute` guarantees; the design does **not** depend on it for correctness.

## What the adapter reconciles

One `HTTPRoute` per deployment. **The gateway always performs the weighted revision selection; the
activator never reimplements it.**

- **Traffic splitting** — weighted `backendRefs` across the per-revision Services. The gateway does
  the weighting; canary/blue-green/rollback are weight edits with no new revision. (Gateway API uses
  relative `weight`; we expose `percent` and translate.)
- **Cold endpoint flip (SKS-style) — at the endpoint layer, not the route.** Route backendRefs are
  **fixed**; what changes is the revision's *routable Service endpoints*. That Service is
  **endpoint-managed** (selectorless): its endpoints are the revision's ready pods when warm, and the
  [deployments-activator](deployments-activator.md) pods when the revision has zero ready pods or is
  draining. The weighted split is unaffected, and because the activator is *already a member of the
  endpoint set* during the cold/draining window, there is **no route swap and no controller-specific
  failover to race** (resolving the stale-endpoint gap portably, with plain `EndpointSlice`
  management). The reconciled `EndpointSlice` carries the right **target port** per endpoint set — the
  workload's `Port` for pod endpoints, the activator's listen port for activator endpoints — so a flip
  never produces unroutable traffic. This is exactly Knative's ServerlessService public-Service
  mechanism.
- **Revision identity — the one Extended dependency.** For the cold/async cases the shared activator
  must know which revision the gateway picked, so each weighted `backendRef` carries
  `X-Revision: <name>` via a `RequestHeaderModifier` — **`set`, never `add`**, so a client-supplied
  `X-Revision` is always overwritten at the edge. Header modification *per backendRef* is Gateway API
  **Extended** (Core only guarantees it at rule level), so this requires a controller with Extended
  backendRef-filter support — **Traefik (verified: `backendRefs[].filters` RequestHeaderModifier,
  Gateway API v1.5.1)**, Envoy Gateway, Istio. (A controller lacking it would need the heavier shape
  of a per-revision activator Service so the target itself encodes the revision.)
- **Async routing** — a second rule matching `Prefer: respond-async` mirrors the *same weighted
  backendRefs*, but every target resolves to the activator (each still tagged with its `X-Revision`).
  The match is a **case-insensitive single-token regex** (`(?i)^respond-async$`) — RFC 7240 tokens
  are case-insensitive, and a casing difference must not silently serve sync. Regex header match is
  Extended, the same tier as the RequestHeaderModifier this design already requires. `Prefer` is an
  RFC 7240 list header, so combined forms like `respond-async, wait=100` are still **not
  recognized** — a documented API restriction.
  So async **respects the split** — the gateway picks the revision by weight; the activator reads
  `X-Revision` and forwards/buffers for *that* revision. So a 90/10 canary whose 10% revision is cold
  still sends ~10% to the activator-for-that-revision, never to the wrong one.

## Configurable URLs

The endpoint hostname is **caller-controllable** via the spec's `host` field → written to
`HTTPRoute.spec.hostnames`; omitted → auto-assigned `{id}.{domain}` (operator-config base domain).

- **Deployments:** the host is stable **across revisions** — the deployment owns it and splitting
  happens on the backends behind it, so rollouts/rollbacks never change the URL.
- **Pools:** pinning `id` + `host` survives re-activation — `deactivate`/`activate` onto a different
  warm pod is reachable at the *same* address while the backing pod is ephemeral.

A host is owned by one deployment/activation (label on the `HTTPRoute`); a collision is rejected with
`409`. `id`/`host` are validated RFC-1123 (label / subdomain) since they become object names /
hostnames. TLS per host is the gateway's `Listener` (e.g. cert-manager) — out of scope here.

On **Docker** there is no gateway: the in-process deployments-activator is always on the path, and `host` selects
its virtual-host routing.

## Controller portability

We emit only standard `HTTPRoute` + plain `EndpointSlice` management, so **routing, readiness, traffic
splitting, and the cold endpoint flip are portable** across conformant controllers. The single
non-Core dependency is **per-backendRef `RequestHeaderModifier`** (Gateway API **Extended**) for the
`X-Revision` tag that the canary cold/async path needs — supported by Traefik, Envoy Gateway, and
Istio. A controller without it can still run plain (non-canary) serving; full canary-with-pooling
would need the per-revision-activator-Service fallback shape. Anything beyond routing
(external-auth/rate-limit) is deferred and out of scope.
