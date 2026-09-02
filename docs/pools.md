# Warm Pod Pools

A pool is standing warm capacity for ordinary [deployment revisions](deployments.md) or [sandboxes](sandboxes.md). The operator declares a fixed pod shape; users continue to submit complete image-backed workload specs. When the fixed fields match exactly, the consumer claims a warm bare pod instead of waiting for scheduling and image pull.

Pools have no public API or user-visible IDs. `POST /v1/deployments` and `POST /v1/sandbox` remain the only create surfaces; their returned state never reveals whether acquisition was warm or direct.

```json
{
  "id": "api",
  "image": "node:22-slim",
  "port": 3000,
  "cpu": 1,
  "memory": 512,
  "command": "node /workspace/server.js",
  "replicas": 2
}
```

The match key is everything that already exists on a warm pod: image, port, CPU, memory, runtime class, volumes, mount capability, termination grace period, and the default workspace. Image tags compare as strings, so pin digests when identical bytes matter. A request with a custom workspace, an omitted command, or kubelet liveness/startup probes takes the direct path because those properties cannot be late-bound safely. Command, environment, artifacts, request timeout, concurrency, hosts, the sidecar readiness probe, replica policy, and autoscaling remain request-time fields.

Deployment pools must declare positive CPU and memory, must not declare command or environment defaults, and must have unique fixed shapes. Both components fail fast on ambiguous or non-matchable configuration.

Each desired Revision slot atomically claims one warm pod through its sidecar. The pod then carries the normal `deployment.revision` and `deployment.replica-slot` labels and a controller owner reference to the Revision. From that point routing and lifecycle are identical to a directly created revision pod. A scaled-down, failed, or deleted claimed pod is discarded, never returned to the warm set; inventory reconciliation replenishes the pool off the request path.

When a matching pool has no warm pod, its `burst` policy chooses the acquisition path, never API availability:

- `cold` (default) creates a pool-shaped pod, waits for it to become claimable, and binds it to the Revision.
- `reject` declines the warm acquisition and the Revision immediately creates its retained direct template instead. It does not return `429` to a deployment user.

Every Revision retains the complete direct template plus the late-bind claim payload. Removing a pool, exhausting it, or scaling a Revision after the pool disappears therefore falls back safely to direct Pod creation. Claimed and directly created pods may coexist in one Revision because their requested shape is identical.

Changing the deployment spec mints a new immutable Revision. Changing operator pool capacity does not change the deployment spec or require client coordination. Rollout and traffic-cut behavior are unchanged.

Configure pools through `deployments.pools` in Helm; see [operations](operations.md#pools). Pools require the Kubernetes backend.

Sandbox pools use `sandbox.pools` and the same fixed-shape key. Sandbox command, environment, artifacts, extra ports, and timeouts are late-bound; a complete request with no match creates directly. Sandbox and Revision inventories remain separate workload kinds even though the matching and controller machinery are shared.

Pool responsibilities are deliberately split. Consumer services perform request-path claim/bind/readiness and own claimed-workload lifecycle. The generic `pool-controller` owns bare warm-pod inventory for both Revision and sandbox pools: it creates replacements and removes poisoned, orphaned, obsolete, or shape-stale unclaimed pods. The chart runs the same binary independently for each enabled pool kind, preserving each plane's placement and S3 credentials. It keeps running with an empty pool list so removing the final pool also cleans up its standing capacity; claimed pods are never reclaimed by inventory cleanup.
