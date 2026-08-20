# Pools Guide

A **pool** is standing warm capacity: a fleet of pre-started pods for a fixed runtime image, kept idle and ready. An **activation** claims one warm pod and late-binds your payload onto it — artifacts, environment, command — skipping scheduling and image pull. Where a cold container start takes seconds, an activation typically completes in well under one.

Pools are **operator configuration**, not an API resource: adding, resizing, or removing a pool is a config change and rollout (see [operations](operations.md#pools)). The API over pools is read + activate.

- [Reading pools](#reading-pools)
- [Activations](#activations)
- [Artifacts](#artifacts)
- [Async activations](#async-activations)
- [When the pool is empty: burst policy](#when-the-pool-is-empty-burst-policy)
- [Activation lifecycle](#activation-lifecycle)

## Reading pools

```bash
curl http://localhost:8080/v1/deployment-pools
```

```json
{"pools": [{"id": "py", "image": "python:3.12-slim", "size": 4, "warm": 4, "claimed": 0}]}
```

`warm` counts pods free to claim right now; `claimed` counts running activations. `GET /v1/deployment-pools/{id}` returns one pool.

## Activations

An **activation** turns a warm pod into a live HTTP endpoint running your command. The call blocks until the workload is serving and returns its URL:

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/node/activations \
  -H "Content-Type: application/json" \
  -d '{"id": "preview-7", "command": "node server.js", "idleTimeoutSeconds": 600}'
```

```json
{"id": "preview-7", "poolId": "node", "status": "ready", "url": "http://preview-7.pools.example.com"}
```

`201 Created`. The activation gets a host (`{id}.{pool domain}`, or set `"host"` yourself) routed through the same gateway as deployments; the pool's configured `port` is the container port it serves on.

The activation `id` is optional (one is generated) but choosing one gives you idempotency: re-POSTing an existing id is `409`. `timeoutSeconds` bounds each request to the activation (default 300, max 3600); `idleTimeoutSeconds` tears the activation down after that long with no traffic (`0` = keep serving until you `DELETE` it).

Run-to-completion workloads belong to the [jobs API](jobs.md), not pools.

## Artifacts

Activations accept the same [artifact schema as jobs](jobs.md#artifacts), materialized into the pod's `/workspace` before your command runs:

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/py/activations \
  -H "Content-Type: application/json" \
  -d '{
    "command": "python /workspace/main.py",  # a server listening on the pool port
    "artifacts": [
      {"id": "code", "type": "download", "in": "https://acme.test/bundle.tar.gz", "out": "bundle.tar.gz"},
      {"id": "unpack", "type": "unarchive", "in": "bundle.tar.gz", "out": ".", "depends": "code"}
    ]
  }'
```

If artifact materialization fails, the activation is reported `failed` with the reason, and the pod is **poisoned** — discarded and replaced, never handed to another activation.

An activation that ERRORS rather than reporting `failed` leaves nothing behind either: the pod, the Service, and the route are all removed before the error returns, including when the error is your own client hanging up. There is no id to clean up afterwards, which is the point — you never received one.

A [`mount`](jobs.md#mount-artifact) also works, on a pool that declares the capability:

```yaml
pools:
  - id: restore
    image: node:22-slim
    mounts: true      # privileged sidecar + a propagating workspace
```

It is a pool dimension rather than a per-activation field because it changes the pod, and warm pods are built before any claim arrives. The mount is established after its image is materialized and before your command is signalled. The cost is a privileged container in every pod of that pool — [the sandbox guide](sandboxes.md#mounting-a-filesystem-image) explains the mechanism and the trade in full, and activations follow exactly the same rules over the same warm machinery.

## Async activations

Add `Prefer: respond-async` and a `callback` to get `202` immediately; the result — the serving URL, or the failure — arrives as an `orchestrator.pool.activation.result` CloudEvent; see [callbacks](callbacks.md).

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/node/activations \
  -H "Content-Type: application/json" -H "Prefer: respond-async" \
  -d '{"command": "node server.js",
       "callback": {"url": "https://acme.test/hook", "key": "signing-secret"}}'
# 202 {"poolId": "node", "status": "activating"}
```

Delivery is at-most-once; nothing is stored for polling while the activation is in flight.

<a id="burst-policy"></a>
## When the pool is empty: burst policy

Each pool declares what happens when an activation arrives and no warm pod is free:

- **`cold`** (default) — the orchestrator creates a pod on demand and pays the cold start (bounded at ~2 minutes) before claiming it. Right when completing is worth more than completing fast.
- **`reject`** — the activation fails fast with `429`. Right for latency-sensitive callers who would rather retry elsewhere than wait.

Either way the pool replenishes itself back to `size` off the request path — a burst never permanently shrinks the pool.

## Activation lifecycle

| `status` | Meaning |
| --- | --- |
| `activating` | Claimed; artifacts materializing / workload starting |
| `ready` | Serving on its URL |
| `failed` | Artifacts failed, the workload exited, or it never became ready |
| `deactivating` | Teardown in progress |

```
GET    /v1/deployment-pools/{id}/activations         # list live activations
GET    /v1/deployment-pools/{id}/activations/{aid}   # one activation's status
DELETE /v1/deployment-pools/{id}/activations/{aid}   # deactivate → 204
```

A claimed pod is **never reused**: deactivation discards it, and the pool replenishes with a fresh warm pod.

Pools require the Kubernetes backend; the Docker development backend serves deployments and jobs only.
