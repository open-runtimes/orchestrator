# Pools Guide

A **pool** is standing warm capacity: a fleet of pre-started pods for a fixed runtime image, kept idle and ready. An **activation** claims one warm pod and late-binds your payload onto it — artifacts, environment, command — skipping scheduling and image pull. Where a cold container start takes seconds, an activation typically completes in well under one.

Pools are **operator configuration**, not an API resource: adding, resizing, or removing a pool is a config change and rollout (see [operations](operations.md#pools)). The API over pools is read + activate.

- [Reading pools](#reading-pools)
- [Exec activations](#exec-activations)
- [HTTP activations](#http-activations)
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

## Exec activations

An **exec pool** (no `port` in its config) runs a command to completion. The call is synchronous — it blocks until the workload exits and returns the result inline:

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/py/activations \
  -H "Content-Type: application/json" \
  -d '{"id": "run-42", "command": "python -c \"print(6*7)\"", "timeoutSeconds": 60}'
```

```json
{
  "id": "run-42",
  "poolId": "py",
  "podId": "pool-py-3746b24347",
  "status": "exited",
  "exitCode": 0,
  "output": "42\n"
}
```

`201 Created`. `output` is the workload's combined stdout+stderr, capped at 1 MiB. A non-zero exit is still a successful *activation* — you get `"exitCode": 3` and the output, not an HTTP error.

The activation `id` is optional (one is generated) but choosing one gives you idempotency: re-POSTing an existing id is `409`. `timeoutSeconds` bounds the run (default 300, max 3600); on timeout the pod is discarded and the activation reported `failed`.

## HTTP activations

An **HTTP pool** (configured with a `port`) turns a warm pod into a live HTTP endpoint. The call returns once the workload is serving:

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/node/activations \
  -H "Content-Type: application/json" \
  -d '{"id": "preview-7", "command": "node server.js", "idleTimeoutSeconds": 600}'
```

```json
{"id": "preview-7", "poolId": "node", "status": "ready", "url": "http://preview-7.pools.example.com"}
```

The activation gets a host (`{id}.{pool domain}`, or set `"host"` yourself) routed through the same gateway as deployments. For HTTP pools, `timeoutSeconds` bounds each request, and `idleTimeoutSeconds` tears the activation down after that long with no traffic (`0` = keep serving until you `DELETE` it).

HTTP pools require the Kubernetes backend.

## Artifacts

Activations accept the same [artifact schema as jobs](jobs.md#artifacts), materialized into the pod's `/workspace` before your command runs:

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/py/activations \
  -H "Content-Type: application/json" \
  -d '{
    "command": "python /workspace/main.py",
    "artifacts": [
      {"id": "code", "type": "download", "in": "https://acme.test/bundle.tar.gz", "out": "bundle.tar.gz"},
      {"id": "unpack", "type": "unarchive", "in": "bundle.tar.gz", "out": ".", "depends": "code"}
    ]
  }'
```

If artifact materialization fails, the activation is reported `failed` with the reason, and the pod is **poisoned** — discarded and replaced, never handed to another activation.

## Async activations

Add `Prefer: respond-async` and a `callback` to get `202` immediately; the result arrives as an `orchestrator.pool.activation.result` CloudEvent (exit code and output for exec pools, the URL for HTTP pools) — see [callbacks](callbacks.md).

```bash
curl -X POST http://localhost:8080/v1/deployment-pools/py/activations \
  -H "Content-Type: application/json" -H "Prefer: respond-async" \
  -d '{"command": "python train.py", "timeoutSeconds": 1800,
       "callback": {"url": "https://acme.test/hook", "key": "signing-secret"}}'
# 202 {"poolId": "py", "status": "activating"}
```

Delivery is at-most-once; nothing is stored for polling while the activation is in flight.

## When the pool is empty: burst policy

Each pool declares what happens when an activation arrives and no warm pod is free:

- **`cold`** (default) — the orchestrator creates a pod on demand and pays the cold start (bounded at ~2 minutes) before claiming it. Right when completing is worth more than completing fast.
- **`reject`** — the activation fails fast with `429`. Right for latency-sensitive callers who would rather retry elsewhere than wait.

Either way the pool replenishes itself back to `size` off the request path — a burst never permanently shrinks the pool.

## Activation lifecycle

| `status` | Meaning |
| --- | --- |
| `activating` | Claimed; artifacts materializing / workload starting |
| `ready` | HTTP pools: serving on its URL |
| `exited` | Exec pools: workload finished (`exitCode` set) |
| `failed` | Artifacts failed, timeout, or the workload never became ready |
| `deactivating` | Teardown in progress |

```
GET    /v1/deployment-pools/{id}/activations         # list live activations
GET    /v1/deployment-pools/{id}/activations/{aid}   # one activation's status
DELETE /v1/deployment-pools/{id}/activations/{aid}   # deactivate → 204
```

A claimed pod is **never reused**: deactivation (or exec completion + retention) discards it, and the pool replenishes with a fresh warm pod. Finished exec activations remain readable via `GET` until retention garbage-collects them.

On the Docker backend, pools support exec activations only, and activation specs (callback, artifacts) don't survive an orchestrator restart — results already delivered are unaffected.
