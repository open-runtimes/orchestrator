# Deployments Guide

A **deployment** is a container serving HTTP behind the orchestrator's gateway. You declare what to run; the orchestrator keeps it running, routable, and scaled — including down to zero.

This guide covers the API from a consumer's perspective. For running the orchestrator itself, see [operations](operations.md).

- [Creating a deployment](#creating-a-deployment)
- [The request spec](#the-request-spec)
- [Status and lifecycle](#status-and-lifecycle)
- [Revisions and updates](#revisions-and-updates)
- [Traffic: canary, rollback, release](#traffic-canary-rollback-release)
- [Autoscaling and scale-to-zero](#autoscaling-and-scale-to-zero)
- [Async requests](#async-requests)
- [Timeouts and failure semantics](#timeouts-and-failure-semantics)
- [Docker backend differences](#docker-backend-differences)

## Creating a deployment

```bash
curl -X POST http://localhost:8080/v1/deployments \
  -H "Content-Type: application/json" \
  -d '{"id": "web", "image": "traefik/whoami", "port": 80}'
```

```json
{
  "id": "web",
  "status": "pending",
  "url": "http://web.localhost",
  "revisions": ["web-00001"],
  "traffic": [{"revisionName": "web-00001", "percent": 100}],
  "mode": "auto",
  "desiredReplicas": 1,
  "availableReplicas": 0
}
```

`201 Created` on first create, `200 OK` when the POST updates an existing deployment — the endpoint is a declarative **apply**: POST the same `id` again with a changed spec and the orchestrator rolls out a new revision. Re-POSTing an identical spec is a no-op.

Once `status` is `ready`, requests reach the workload through the gateway by host:

```bash
curl -H "Host: web.localhost" http://<gateway>/
```

A deployment serves on one or more **hosts**. By default it gets `{id}.{domain}` (the operator configures the domain); set `"hosts"` explicitly to use your own — the first entry is the primary (it's what `url` reports), and every entry routes to the same revisions and traffic table:

```json
{"id": "web", "image": "ghcr.io/acme/web:v3", "port": 8080,
 "hosts": ["acme.com", "www.acme.com"]}
```

Each host is owned by exactly one deployment — claiming a host another deployment already owns is rejected with `409`.

## The request spec

```json
{
  "id": "web",                    // required — RFC-1123 label, ≤63 chars; part of object names
  "image": "ghcr.io/acme/web:v3", // required
  "port": 8080,                   // required — the container port serving HTTP
  "command": "server --flag",     // optional — overrides the image entrypoint
  "cpu": 1,                       // cores (limit); default 1
  "memory": 512,                  // MB (limit); default 512
  "environment": {"KEY": "value"},
  "workspace": "/workspace",      // working directory + shared-volume mount path; default /workspace
  "hosts": ["web.example.com"],   // hosts[0] is the primary; default [{id}.{domain}]
  "replicas": 2,                  // fixed count when not autoscaling; default 1
  "concurrency": 50,              // hard per-replica in-flight cap; 0 = unlimited
  "autoscaling": {                // see Autoscaling below
    "minReplicas": 0,
    "maxReplicas": 10,
    "target": 100
  },
  "probes": {
    "readiness": {"path": "/healthz", "periodMillis": 500, "timeoutMillis": 200, "failureThreshold": 3},
    "liveness":  {"path": "/healthz"},
    "startup":   {"path": "/healthz"}
  },
  "artifacts": [                  // materialized into the workspace before serving
    {"id": "cfg", "type": "write", "in": "...", "out": "config.yaml"}
  ],
  "callback": {                   // required for async requests — see Async below
    "url": "https://acme.test/hook",
    "key": "signing-secret"
  },
  "runtimeClass": "gvisor",       // isolation tier: runc (default) | gvisor | kata — K8s only
  "timeoutSeconds": 300,              // per-request total → 504; default 300
  "startTimeoutSeconds": 300, // wait for capacity on a cold start → 503; default 300
  "readyTimeoutSeconds": 600      // ready deadline before a rollout is marked failed; default 600
}
```

Unknown fields are rejected with `400` naming the field, so a typo (`"replcias"`) fails loudly instead of silently deploying defaults.

Probes: the **readiness** probe is run by the orchestrator's sidecar and honors sub-second periods — it gates whether a replica receives traffic. Liveness and startup probes are kubelet-run at whole-second granularity. Omitting `path` makes a probe a TCP connect check.

Artifacts use the same schema as [jobs](jobs.md#artifacts) and run before the workload starts serving. Every type except [`mount`](jobs.md#mount) is available: a mount needs a post phase, and a serving workload has none.

## Status and lifecycle

`GET /v1/deployments/{id}` (and `GET /v1/deployments` for the list):

| `status` | Meaning |
| --- | --- |
| `pending` | Rolling out; no ready replica yet |
| `ready` | At least one replica serving |
| `idle` | Scaled to zero — the next request cold-starts it |
| `degraded` | Fewer ready replicas than desired |
| `failed` | Rollout did not become ready within `readyTimeoutSeconds`, or the workload crashed |
| `deleting` | Teardown in progress |

`DELETE /v1/deployments/{id}` returns `204` and tears everything down; the host stops resolving immediately.

## Revisions and updates

Every spec change mints an immutable **revision**, named `{id}-00001`, `{id}-00002`, … Revisions are individually routable, which is what makes canaries and rollbacks cheap.

In the default **auto** mode, a new revision takes 100% of traffic *once it reports ready* — the previous revision keeps serving until then, so a bad image never blacks out the host. The most recent revisions are retained (default 3, plus any still receiving traffic) for rollback; older ones are garbage-collected.

`GET /v1/deployments/{id}/revisions` lists revisions (newest first) with the current traffic table.

## Traffic: canary, rollback, release

`POST /v1/deployments/{id}/traffic` pins an explicit split across existing revisions. Percents must sum to 100.

```bash
# Canary: 90% stable, 10% new
curl -X POST http://localhost:8080/v1/deployments/web/traffic \
  -H "Content-Type: application/json" \
  -d '{"targets": [
        {"revisionName": "web-00001", "percent": 90},
        {"revisionName": "web-00002", "percent": 10}
      ]}'
```

Setting any split switches the deployment to **manual** mode (visible as `"mode": "manual"` in status): new revisions still build, but traffic no longer auto-cuts — your split stays exactly where you put it. Rollback is just a split: `[{"revisionName": "web-00001", "percent": 100}]`.

When you're done, **release** back to auto with an empty target list:

```bash
curl -X POST http://localhost:8080/v1/deployments/web/traffic \
  -H "Content-Type: application/json" -d '{"targets": []}'
# → 100% on the latest revision, "mode": "auto", auto-cut re-armed
```

(Posting exactly `[{latest revision, 100}]` also releases — the empty list just saves you looking up the revision name.)

## Autoscaling and scale-to-zero

Without `autoscaling`, a deployment runs a fixed `replicas` count. With it:

```json
"autoscaling": {"minReplicas": 0, "maxReplicas": 10, "target": 100}
```

- The autoscaler tracks **concurrency** — average in-flight requests per replica — over a sliding window, and sizes the deployment to `ceil(avg / target)`, clamped to `[minReplicas, maxReplicas]`.
- `target` defaults to 100; `maxReplicas` defaults to `max(replicas, 1)`.
- `minReplicas: 0` enables **scale-to-zero**: after a window with no traffic the deployment idles (status `idle`), costing nothing.

A request arriving while idle is **held, not failed**: the orchestrator buffers it, scales the deployment back up, and forwards the request to the first replica that becomes reachable. The client just sees a slower response (typically a few seconds, bounded by `startTimeoutSeconds`). Traffic splits and cold starts compose — a canary whose 10% leg is cold still sends that 10% to the right revision.

`concurrency` (the hard cap) and `autoscaling.target` (the scaling goal) are independent: the cap rejects excess load per replica; the target adds replicas before the cap is reached.

## Async requests

Send `Prefer: respond-async` on any request to the deployment's host and the gateway accepts it immediately:

```bash
curl -X POST -H "Host: web.localhost" -H "Prefer: respond-async" \
  http://<gateway>/render -d '{"frames": 900}'
# 202 Accepted
# X-Invocation-Id: 7f3a9c...
```

The request is executed in the background (cold-starting the deployment if needed) and the response is delivered to the deployment's **callback** as an `orchestrator.deployment.response` CloudEvent carrying the status code, body, and the `X-Invocation-Id` for correlation — see [callbacks](callbacks.md). Non-UTF-8 response bodies arrive base64-encoded with `"bodyEncoding": "base64"`; bodies over 1 MiB are truncated and flagged.

Notes:

- Async **requires** a `callback` on the deployment spec — without one the request is rejected with `400`.
- The preference token is matched case-insensitively, but combined RFC 7240 forms (`respond-async, wait=10`) are not recognized and are served synchronously.
- Delivery is at-most-once: nothing is stored, and `X-Invocation-Id` is a correlation ID, not a polling handle.
- Send your own `X-Invocation-Id` on the request to set the correlation id (echoed on the `202` and carried in the callback); omit it and the orchestrator generates one.
- The callback echoes the request's `requestMethod`, `requestPath`, and `requestHeaders`, so the result is self-describing without keeping any local state keyed by the invocation id.
- Request bodies are buffered up to 10 MiB (`413` beyond that).

## Timeouts and failure semantics

| Setting | Bounds | Client sees |
| --- | --- | --- |
| `startTimeoutSeconds` | Waiting for a ready replica (cold start, crash recovery) | `503` if nothing becomes ready in time |
| `timeoutSeconds` | The whole request once a replica has it | `504` |
| `readyTimeoutSeconds` | A rollout reaching ready | status `failed`; previous revision keeps serving |

An unknown host at the gateway is `404`. A replica crash mid-rollout leaves the previous revision serving; a crash of the *only* replica behaves like a cold start — the next request raises the deployment again.

## Docker backend differences

The Docker backend (`ORCHESTRATOR_BACKEND=docker`) is the dev-parity implementation. Differences from Kubernetes:

- **Single revision**: applies replace in place; there is no revision history, so traffic splitting returns `400` (100% to the deployment itself, or an empty release, are accepted no-ops).
- **One replica**: `replicas` is clamped to 1; autoscaling honors only 0↔1 (scale-to-zero still works).
- **No isolation tiers**: `"runtimeClass"` other than `runc` is rejected.
- The data plane is served by the orchestrator's own listener rather than a gateway — same host-based routing, same async semantics.
