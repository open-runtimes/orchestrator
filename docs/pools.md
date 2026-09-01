# Revision Pools

A deployment pool is standing warm capacity for ordinary [deployment revisions](deployments.md). The operator declares a fixed pod shape; a deployment selects it with `pool` instead of `image`. Its `Revision` then claims warm pods for its replica slots rather than creating new pods and waiting for scheduling and image pull.

Pools have no public API and no activation resources. `POST /v1/deployments` remains the only deployment create/update surface, and the resulting Revision owns rollout, status, autoscaling, routing, and pod deletion whether its pods were created or claimed.

```json
{
  "id": "api",
  "pool": "node",
  "command": "node /workspace/server.js",
  "replicas": 2
}
```

The pool fixes everything that already exists on a warm pod: image, port, CPU, memory, runtime class, volumes, mount capability, termination grace period, and kubelet probes. A request may late-bind command, environment, artifacts, request timeout, concurrency, hosts, a sidecar readiness probe, replica policy, and autoscaling. Passing a conflicting pod-shape field is rejected.

Each desired Revision slot atomically claims one warm pod through its sidecar. The pod then carries the normal `deployment.revision` and `deployment.replica-slot` labels and a controller owner reference to the Revision. From that point routing and lifecycle are identical to a directly created revision pod. A scaled-down, failed, or deleted claimed pod is discarded, never returned to the warm set; inventory reconciliation replenishes the pool off the request path.

When no warm pod is available, the pool's `burst` policy applies:

- `cold` (default) creates a pool-shaped pod, waits for it to become claimable, and binds it to the Revision.
- `reject` leaves the Revision pending with a capacity failure and retries through normal reconciliation.

Changing a deployment's selected pool or late-bound payload mints a new immutable Revision. Rollout and traffic-cut behavior are unchanged.

Configure pools through `deployments.pools` in Helm; see [operations](operations.md#pools). Pools require the Kubernetes backend.
