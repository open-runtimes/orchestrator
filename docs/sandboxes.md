# Sandboxes Guide

A **sandbox** is a live, isolated workspace you drive from the outside: create one, run commands in it, read and write its files, tear it down. Where a [job](jobs.md) runs to completion and a [deployment](deployments.md) serves traffic under a stable name, a sandbox does neither — it sits there and waits for you. It is the shape an agent, a notebook kernel, or an interactive build wants.

There are two ways to get one. Name a **sandbox pool** and you claim a pod that is already running — standing warm capacity, the same claim-and-late-bind machinery [pools](pools.md) use, so a create is sub-second. Name an **image** instead and the pod is created for you: no standing capacity to configure, at the cost of a cold start, and the sandbox takes its shape from your request rather than from an operator's config. Deployments work the same way, and this is the same trade.

One thing to know before anything else: **exec and files are not part of this API.** They are an HTTP contract served *inside* the sandbox, reached at the sandbox's own URL. This API creates, inspects, and tears down sandboxes; it never sits between you and your commands. See [the sandbox contract](#the-sandbox-contract).

- [Endpoints](#endpoints)
  - [A sandbox with no pool](#a-sandbox-with-no-pool)
- [The sandbox contract](#the-sandbox-contract)
- [Artifacts](#artifacts)
  - [Mounting a filesystem image](#mounting-a-filesystem-image)
- [Ports](#ports)
- [Persistence](#persistence)
- [Isolation](#isolation)
- [Lifecycle](#lifecycle)
- [Limits](#limits)
- [Error responses](#error-responses)
- [Complete example](#complete-example)
- [Sandbox pools](#sandbox-pools)
- [Design notes](#design-notes)

## Endpoints

| | | |
| --- | --- | --- |
| `POST` | `/v1/sandbox` | Create a sandbox → `201` |
| `GET` | `/v1/sandbox` | List live sandboxes → `200` |
| `GET` | `/v1/sandbox/{id}` | One sandbox's status → `200` |
| `DELETE` | `/v1/sandbox/{id}` | Tear it down → `204` |
| `GET` | `/v1/sandbox-pool` | List sandbox pools with live counts → `200` |
| `GET` | `/v1/sandbox-pool/{id}` | One pool → `200` |

All of them require `Authorization: Bearer <API key>` when one is configured, and take `Content-Type: application/json` on writes. Unknown fields in a body are rejected rather than ignored, so a typo fails loudly instead of silently creating a sandbox with defaults.

There is no exec or files endpoint here, and there never will be — see [the sandbox contract](#the-sandbox-contract).

### Create a sandbox

```bash
curl -X POST http://localhost:8080/v1/sandbox \
  -H "Content-Type: application/json" \
  -d '{"pool": "py", "idleTimeoutSeconds": 900}'
```

```json
{
  "id": "py-3f9c1a02",
  "poolId": "py",
  "status": "ready",
  "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com"
}
```

`201 Created`, and the URL is live when you receive it — there is no per-sandbox gateway route to wait on. The call is synchronous because a claim is sub-second; there is no async variant and no callback.

**Treat the URL as a secret.** Anyone who can reach it can run commands in the sandbox, so its hostname is an unguessable 128-bit token rather than your `id` — don't log it, and don't hand it to anyone you wouldn't hand a shell. `DELETE` invalidates it immediately: the proxy stops routing to a pod the moment it is marked for deletion, so the URL fails while the pod is still terminating, and dies with the token when it goes.

Exactly one of `pool` or `image` — both is ambiguous (which image wins?) and neither leaves nothing to run.

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `pool` | string | — | The sandbox pool to claim from: a warm pod, so the create is sub-second. Unknown pool → `404`. |
| `image` | string | — | A runtime image to create a pod from instead, for a sandbox with no pool behind it. The agent is installed into it exactly as into a pool's image, so any image works. |
| `port` | int | — | **Required with `image`.** Where the contract is served — what a pool would otherwise declare. |
| `cpu`, `memory` | number, int | platform default | Size the workload container of a poolless sandbox. |
| `runtimeClass` | string | platform default | Isolation tier (`runc` \| `gvisor` \| `kata`) for a poolless sandbox. Per-sandbox here, unlike a pool's, because the pod is built for this request. |
| `volumes` | array | — | Attach existing storage to a poolless sandbox — per-sandbox for the same reason. Same [schema](#persistence) as a pool's. |
| `id` | string | generated | Caller-chosen for a stable API path and idempotency. RFC-1123 label: lowercase alphanumeric with interior hyphens, ≤63 characters. Re-POSTing a live id → `409`. Generated as `{pool}-{8 hex}` when omitted. **Not** the address — see [the URL is a capability](#the-url-is-a-capability). |
| `command` | string | the pool's | What the claim execs. With none declared anywhere, the sandbox runs the [agent](#the-sandbox-contract) installed in its workspace — the usual case. |
| `environment` | object | — | Environment variables for the workload. |
| `ports` | int[] | — | Extra ports this sandbox serves, each addressable at its own hostname. See [ports](#ports). |
| `artifacts` | array | — | Materialized into the workspace before the sandbox reports ready. Same schema as [job artifacts](jobs.md#artifacts), except [`mount`](jobs.md#mount-artifact). See [artifacts](#artifacts). |
| `timeoutSeconds` | int | `300` | Bounds each request to the sandbox's URL. `0` means **no bound**, for sessions meant to outlive one. Max `3600`. |
| `idleTimeoutSeconds` | int | the pool's `maxIdleSeconds` | Tear the sandbox down after this long with no traffic. `0` = live until `DELETE`, where the pool allows it. Capped by the pool's ceiling. |

The response is a **sandbox status**, the same shape every read returns:

| Field | Type | Notes |
| --- | --- | --- |
| `id` | string | The one you passed, or the generated one. |
| `poolId` | string | The pool it was claimed from. |
| `status` | string | `creating` \| `ready` \| `failed` \| `deleting` — see [lifecycle](#lifecycle). |
| `url` | string | The sandbox's address on its pool's port. Absent when nothing is serving (a `failed` sandbox has no URL). |
| `urls` | object | Every port it serves, keyed by port number as a string, including the pool's own. Read addresses from here rather than building them. |
| `error` | string | Why it failed, when it did. |

### A sandbox with no pool

```bash
curl -X POST http://localhost:8080/v1/sandbox \
  -H "Content-Type: application/json" \
  -d '{"image": "python:3.12-slim", "port": 3000, "runtimeClass": "gvisor"}'
```

```json
{
  "id": "sbx-3f9c1a02",
  "status": "ready",
  "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com"
}
```

No `poolId` in the response: there was no pool. Everything else is identical — the contract, artifacts, extra ports, idle teardown, `status`/`list` reconstruction, `DELETE`.

What you trade:

- **A cold start.** The pod is created, scheduled, pulled if need be, and its agent started before the sandbox is `ready` — seconds, against well under one for a claim. If you create sandboxes at any rate, a pool is what you want.
- **No pool ceiling.** `maxIdleSeconds` belongs to a pool, so a poolless sandbox is bounded only by the `idleTimeoutSeconds` you pass. Omit it and nothing collects the sandbox but a `DELETE`.

What you gain: nothing to configure ahead of time, and per-sandbox control over `runtimeClass`, `volumes`, `cpu`/`memory` and [mounting](#mounting-a-filesystem-image) — all of which a pool fixes for every sandbox in it, because its pods are already running when you claim one.

Under the hood it is a **pool of one**: keyed by the sandbox's own id so its pod is never offered to another sandbox, sized zero so nothing replenishes it, and created by the same burst policy that covers an exhausted pool. Which is why every other rule in this guide applies unchanged.

### Read, list, and delete

```bash
curl http://localhost:8080/v1/sandbox/py-3f9c1a02     # one sandbox's status
curl http://localhost:8080/v1/sandbox                  # {"sandboxes": [ … ]}
curl -X DELETE http://localhost:8080/v1/sandbox/py-3f9c1a02   # 204
```

State is reconstructed from the backend on every read — nothing about a sandbox lives in service memory — so a control-plane restart leaves live sandboxes serving and answering. A `DELETE` is idempotent from the caller's point of view only in that a gone sandbox is `404`; teardown itself is immediate.

## The sandbox contract

**Your image does not have to implement it.** The [`open-runtimes/sandbox`](https://github.com/open-runtimes/sandbox) agent is a static binary, and the orchestrator copies it into every sandbox's workspace at pod creation — the same mechanism that installs the pool shim. A sandbox pool over `node:22-slim`, `python:3.12-slim`, or a distroless image serves the contract with nothing added to the image and no `command` declared:

```yaml
sandbox:
  pools:
    - id: node
      image: node:22-slim   # implements nothing; the agent supplies the contract
      port: 3000
```

A pool or a create call may still set `command` — to run an image that serves the contract itself, or to wrap the agent — and it wins over the installed agent. These are the three routes it answers on the pool's port:

| | |
| --- | --- |
| `POST /execute` | `{"command", "timeoutSeconds"}` → `{"exitCode", "stdout", "stderr", "durationMillis", "truncated"}` |
| `GET\|PUT\|DELETE /files/{path}` | whole-file read / write / remove, relative to the workspace |
| `GET /healthz` | readiness — the pool's probe subject |

That split is deliberate. It keeps the control plane off the data path, so a long exec is not killed by an orchestrator rolling restart and a large file upload is not a shared-fate bottleneck. And because the agent is installed rather than baked in, "bring your own image" costs nothing: a computer-use image, a JVM with a warm classloader, or an image that also speaks a protocol we have never heard of all work — the last of those by declaring its own `command`.

### Running commands

```bash
curl -X POST http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/execute \
  -H "Content-Type: application/json" \
  -d '{"command": "python -c \"print(2+2)\"", "timeoutSeconds": 30}'
```

```json
{"exitCode": 0, "stdout": "4\n", "stderr": "", "durationMillis": 41}
```

For the reference image: commands run in the worker container — same image, same filesystem, same [isolation tier](#isolation) — with the workspace as the working directory. Output is capped at 1 MiB per stream, past which `truncated` is `true`; beyond that, write to a file and read it back.

Note the two timeouts. The one in the body is the agent's bound on the command; the sandbox's own `timeoutSeconds` bounds the HTTP request carrying it. A command that outlives the request bound is cut by the proxy whatever the agent was told.

### Files

```bash
curl -X PUT  http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/files/main.py --data-binary @main.py
curl         http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/files/out.json
```

Paths are relative to the workspace; `..` and absolute paths are `400`. `GET` on a directory lists it as JSON — including the machinery's own `.pool/`, `.sandbox-agent`, `.pool-exec.fifo`, and `.pool-shim.log`, which share the workspace volume. Ignore them; they are inert once the sandbox is serving.

## Artifacts

For anything bulkier than a `PUT`, declare [artifacts](jobs.md#artifacts) at create time — the bulk-in path, materialized into the workspace by the sidecar before the sandbox reports ready:

```bash
curl -X POST http://localhost:8080/v1/sandbox \
  -H "Content-Type: application/json" \
  -d '{
    "pool": "py",
    "artifacts": [
      {"id": "code", "type": "download", "in": "https://acme.test/repo.tar.gz", "out": "repo.tar.gz"},
      {"id": "unpack", "type": "unarchive", "in": "repo.tar.gz", "out": ".", "depends": "code"}
    ]
  }'
```

Artifacts run in the **pre phase only**: a post-phase artifact (one depending on the `"workload"` sentinel) never runs, because a sandbox has no teardown phase to run it in. To keep a workspace across sandboxes, [sync a writable mount](#keeping-a-workspace-across-sandboxes) rather than snapshotting on the way out.

### Mounting a filesystem image

A [`mount`](jobs.md#mount-artifact) puts a squashfs or erofs image into the workspace without extracting it — read-only, or `writable` with an overlay whose size you cap. For a large read-mostly tree that is the difference between an `O(1)` mount and copying every byte in.

From a pool, it needs one that declares the capability:

```yaml
sandbox:
  pools:
    - id: restore
      image: python:3.12-slim
      port: 3000
      mounts: true
```

```jsonc
{
  "pool": "restore",
  "artifacts": [
    {"id": "img",  "type": "download", "in": "s3://acme/base.erofs", "out": "base.erofs"},
    {"id": "tree", "type": "mount", "in": "base.erofs", "out": "work",
     "writable": true, "size": 512, "depends": "img"}
  ]
}
```

The mount is established after the image is materialized and **before** the workload is signalled, so it is in place when your command first runs. A mount that fails poisons the pod: the sandbox is `failed` with the reason and nothing starts.

[A poolless sandbox](#a-sandbox-with-no-pool) needs no such declaration: its pod is built for the request, so the capability is inferred from the artifacts exactly as it is for a [job](jobs.md#mount-artifact) or a [revision](deployments.md#the-request-spec).

**Why it is a pool dimension.** Mounting needs `CAP_SYS_ADMIN` and a loop device, and the mount has to cross from the sidecar that makes it into the container that reads it — so the sidecar runs **privileged as root** and the workspace carries mount propagation. Those are properties of a pod, and a warm pod is built long before your claim arrives. A `mount` against a pool without `mounts: true` is a `400` naming the setting.

[Pool activations](pools.md#artifacts) follow the same rules over the same machinery, and [deployment revisions](deployments.md#the-request-spec) and [jobs](jobs.md#mount-artifact) mount too — they infer the capability per request, because their pods are built for one request rather than standing warm.

**What that costs.** A privileged container sits in every pod of that pool, beside whatever the sandbox runs. If the sandbox is running code you do not trust, that is a boundary you should not lean on: keep untrusted work on pools without `mounts`, and treat a mounting pool as trusted infrastructure. The Docker backend cannot do it at all — sibling containers do not share a mount namespace — and says so rather than failing at claim time. Artifacts keep their own time budget even when the sandbox's requests are unbounded, so `"timeoutSeconds": 0` never means an unbounded download.

If materialization fails, the sandbox is `failed` with the reason and no URL, and its pod is **poisoned** — discarded and replaced, never handed to another sandbox.

### Keeping a workspace across sandboxes

A `writable` mount is scratch by default: the overlay's upper layer is RAM, and it dies with the pod. Give it a `sync` destination and that layer is restored on the way in and pushed on the way out, so the next sandbox opens the workspace where the last one left it.

```jsonc
{
  "pool": "restore",
  "artifacts": [
    {"id": "img",  "type": "download", "in": "s3://acme/base.erofs", "out": "base.erofs"},
    {"id": "tree", "type": "mount", "in": "base.erofs", "out": "work", "depends": "img",
     "writable": true, "size": 512,
     "sync": "s3://acme/workspaces/user-42.tgz",
     "syncIntervalSeconds": 30}
  ]
}
```

Only the **delta** travels. The image stays the base and the upper layer holds exactly what the workload created or changed, so a 2 GiB base with a 40 MiB working set moves 40 MiB, not 2 GiB. A synced upper lives on the workspace volume rather than tmpfs, so it does not count against the pod's memory.

`syncIntervalSeconds` is how much a crash may cost — 60 by default, 5 at the least. The delta is pushed on that interval and once more after the sandbox stops serving, which is why the last push is an optimisation rather than a promise: if the pod is killed outright you lose an interval, not the session. A push is skipped when nothing changed, so an idle sandbox costs a directory walk rather than an upload.

What it is not:

- **Not atomic.** The workload keeps writing while the delta is archived, so an interval push is a crash-consistent restore point, not a snapshot. The final one runs when nothing is serving, so it is clean.
- **Not shared.** Two live sandboxes syncing to the same destination will overwrite each other, last push wins. One destination, one sandbox at a time.
- **Not a filesystem.** Nothing is written through to the destination as it happens; a reader elsewhere sees the last push.
- **Not fine-grained.** Each push carries the whole delta, which grows with the session. A long-lived workspace that writes a lot is the case this does least well.

A restored tree is made writable to whoever the workload runs as, since the sidecar unpacks it as root and cannot know the image's uid. A destination with nothing at it yet is a first session, not an error; a destination that exists and cannot be read **fails the mount**, so a workspace we merely failed to read is never overwritten by an empty one.

`sync` requires `writable` — there is nothing to sync from a read-only mount — and works the same way for [pool activations](pools.md#artifacts), [deployment revisions](deployments.md#the-request-spec) and [jobs](jobs.md#mount-artifact), since it is the same sidecar in all four.

## Ports

A sandbox serves its pool's port — the contract — and any extra ports you declare at create time. Each gets its own hostname, so a dev server, a language server, or a terminal socket is reachable alongside `/execute`:

```bash
curl -X POST http://localhost:8080/v1/sandbox \
  -H "Content-Type: application/json" \
  -d '{"pool": "py", "ports": [5173, 9229]}'
```

```json
{
  "id": "py-3f9c1a02",
  "poolId": "py",
  "status": "ready",
  "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com",
  "urls": {
    "3000": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com",
    "5173": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f-5173.sandboxes.example.com",
    "9229": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f-9229.sandboxes.example.com"
  }
}
```

Read the addresses out of `urls`; don't build them. Ports are **not** a pool dimension the way [`volumes`](#persistence) and [`runtimeClass`](#isolation) are: a container may bind a port at any time, so nothing about the warm pod fixes them. Nothing needs to be listening when you create the sandbox either — start the dev server from an `/execute` call later and its URL begins working.

Two rules the platform enforces:

- **Only declared ports are reachable.** The port travels in the hostname; the proxy turns it into a hint the sidecar checks against the claim, and the dial happens on loopback inside that sandbox's own pod. A port you did not declare is `404`, and a hint a client sets by hand is discarded.
- **The port shares the token's DNS label** (`s-{token}-5173`), rather than nesting as `s-5173.{token}`. A wildcard certificate covers exactly one label, so the flat form is reachable under one `*.{domain}` cert while the nested form would need a certificate per sandbox.

Readiness is the primary port's alone: a secondary port that never comes up does not fail the sandbox, and traffic to it counts as activity for the [idle timeout](#lifecycle) like any other request. `8000` and `8001` belong to the sidecar and are refused.

WebSocket traffic (terminals, LSP) upgrades cleanly through the proxy. Create those sandboxes with `"timeoutSeconds": 0` — the per-request bound applies to an upgraded connection like any other request, so at the default it would cut the session after five minutes. `0` removes the bound for that sandbox. The same bound decides how long a rolling restart waits for that session before giving up on it, capped by the pod's `PROXY_MAX_DRAIN_SECONDS`.

## Persistence

The workspace is ephemeral: an `emptyDir` that dies with the sandbox. Two opt-ins buy durability:

- **`artifacts`** — bulk in, per sandbox, at create time ([above](#artifacts)).
- **`volumes`** — an existing K8s PVC, the same schema jobs and deployments take. Attach-only: the orchestrator never creates, sizes, or deletes the storage.

  | Field | Type | Notes |
  | --- | --- | --- |
  | `source` | string | **Required.** An existing PVC name (a Docker volume name on the Docker backend). |
  | `path` | string | **Required.** Absolute mount path inside the container. |
  | `subPath` | string | Mount only this subdirectory of the volume. |
  | `readonly` | bool | Mount read-only. |

**On a pool, `volumes` is a pool dimension, not a per-sandbox field** — the same constraint as [`runtimeClass`](#isolation), and for the same reason. ([A poolless sandbox](#a-sandbox-with-no-pool) takes both per request, because its pod is built for it.) A warm pod is already running when you claim it, and its mounts were fixed when it was created; the claim protocol late-binds a command, environment, and artifacts, but it cannot attach storage to a live pod. So the volume is declared on the pool and mounted into every warm pod in that fleet:

```yaml
# values.yaml — operator config, not an API call
sandbox:
  pools:
    - id: py
      image: python:3.12-slim
      volumes:
        - source: agent-scratch
          path: /data
          subPath: tenant-a   # optional: mount a subdirectory of the volume
```

Want per-sandbox storage? Declare a pool per storage shape. Accepting `volumes` on the create call would mean cold-starting a pod for it, which silently turns a sub-second create into a slow one — a bad thing to do quietly.

Nothing is checkpointed and there is no suspend/resume — a sandbox is either running or gone. Anything worth keeping goes in a pool volume, or gets read out through `/files` before teardown.

## Isolation

A sandbox from a pool runs at that pool's isolation tier, set by its `runtimeClass` (`runc` | `gvisor` | `kata`). Warm pods are runtime-fixed at creation, so on a pool this is a **pool dimension, not a per-sandbox field** — warm pools are keyed by `(image, runtimeClass)`. Want gVisor-isolated sandboxes from a pool? Configure a gVisor pool and create from it. [A poolless sandbox](#a-sandbox-with-no-pool) names its own tier, since its pod does not exist until you ask.

Untrusted, model-generated code is the expected workload here, so `gvisor` or `kata` is the right default for a sandbox pool even though `runc` is the platform default. See [operations](operations.md#isolation-tiers) for mapping tiers to your cluster's RuntimeClasses.

## Lifecycle

| `status` | Meaning |
| --- | --- |
| `creating` | Claimed; artifacts materializing |
| `ready` | Contract served at its URL |
| `failed` | Artifacts failed, or the image never became ready |
| `deleting` | Teardown in progress |

A create returns once the sandbox is `ready` or `failed`; `creating` is what a concurrent read sees in between. There is no path back from `failed` — create another.

`idleTimeoutSeconds` tears the sandbox down after that long with no traffic (`0` = live until `DELETE`, where the pool allows it). Traffic on any port counts. A pool's `maxIdleSeconds` caps it and fills it in when omitted: an abandoned sandbox holds a warm pod hostage, so requesting more than the ceiling is a `400`.

As with activations, a used pod is **never reused** — teardown discards it and the pool replenishes with a fresh one.

## Limits

| | |
| --- | --- |
| `id` length | 63 characters (an RFC-1123 label) |
| `ports` per sandbox | 16 |
| `artifacts` per sandbox | 64 |
| `timeoutSeconds` | `0` (unbounded) to `3600`; default `300` |
| `idleTimeoutSeconds` | `0` (until `DELETE`) up to the pool's `maxIdleSeconds` |
| Reserved ports | `8000`, `8001` (the sidecar), and the pool's own port |
| `mount` artifacts | On a pool: only with `mounts: true`. Poolless: inferred from the artifacts. Never on the Docker backend |
| `/execute` output | 1 MiB per stream, then `truncated` (reference agent) |

Concurrent sandboxes are bounded by your pools' `size` and [burst policy](pools.md#burst-policy), not by a limit here: a create against an exhausted pool either cold-starts a pod or is rejected with `429`, depending on the pool's `burst`.

## Error responses

All errors return JSON:

```json
{
  "error": "port 8000 is reserved by the sandbox sidecar"
}
```

| Status | Meaning |
| --- | --- |
| 400 | Invalid request — malformed JSON, unknown field, or failed validation; the message names the offending field |
| 401 | Missing or invalid API key |
| 404 | Sandbox not found, or the `pool` named does not exist |
| 409 | A sandbox with this `id` is already live |
| 415 | `Content-Type` is not `application/json` |
| 429 | The pool had no warm pod and its burst policy is `reject` |
| 500 | Internal error |

A `failed` sandbox is **not** an error response: the create returns `201` with `"status": "failed"` and an `error` field, because the sandbox exists as a record you can read and delete. Errors above mean nothing was created.

Inside the sandbox, the contract has its own responses — `400` for a path outside the workspace, `404` for a missing file, and the agent's own codes for `/execute`. Those come from the image, not from this API.

## Complete example

An agent workspace: code in via artifacts, a dev server on its own hostname, an unbounded session, then teardown.

```bash
# 1. Create. timeoutSeconds 0 because the agent holds a long-lived connection.
curl -sX POST http://localhost:8080/v1/sandbox \
  -H "Content-Type: application/json" \
  -d '{
    "id": "agent-run-42",
    "pool": "py",
    "ports": [5173],
    "timeoutSeconds": 0,
    "idleTimeoutSeconds": 900,
    "artifacts": [
      {"id": "code", "type": "download", "in": "https://acme.test/app.tar.gz", "out": "app.tar.gz"},
      {"id": "unpack", "type": "unarchive", "in": "app.tar.gz", "out": ".", "depends": "code"}
    ]
  }'
```

```json
{
  "id": "agent-run-42",
  "poolId": "py",
  "status": "ready",
  "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com",
  "urls": {
    "3000": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com",
    "5173": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f-5173.sandboxes.example.com"
  }
}
```

```bash
SBX=http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com

# 2. Run something. The code from the artifacts is already in the workspace.
curl -sX POST $SBX/execute -H "Content-Type: application/json" \
  -d '{"command": "python -m pytest -q", "timeoutSeconds": 120}'

# 3. Write a file in, read a result out.
curl -sX PUT $SBX/files/config.json --data-binary @config.json
curl -s $SBX/files/report.xml -o report.xml

# 4. Start the dev server; its hostname begins working when it binds.
curl -sX POST $SBX/execute -H "Content-Type: application/json" \
  -d '{"command": "nohup npm run dev -- --port 5173 &"}'
curl -s http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f-5173.sandboxes.example.com/

# 5. Done. This invalidates the URL along with the pod.
curl -sX DELETE http://localhost:8080/v1/sandbox/agent-run-42
```

Had step 5 never come, `idleTimeoutSeconds: 900` would have collected it fifteen minutes after the last request on any port.

## Sandbox pools

Sandbox pools are **operator configuration**, not an API resource: adding, resizing, or removing one is a config change and a rollout. The API over them is read-only.

```bash
curl http://localhost:8080/v1/sandbox-pool
```

```json
{"pools": [{"id": "py", "image": "python:3.12-slim", "size": 4, "warm": 4, "claimed": 1}]}
```

`GET /v1/sandbox-pool/{id}` returns one pool. On Docker, `warm` is always `0` — see [the Docker backend](#the-docker-backend). Pools are optional: the domain alone enables sandboxes, and [a poolless create](#a-sandbox-with-no-pool) needs none. They are configured exactly like [deployment pools](operations.md#pools) — `size`, `cpu`, `memory`, `runtimeClass`, `burst`, `volumes`, `port` — plus an optional `command` (overriding the installed agent) and `maxIdleSeconds`. They are a separate fleet because their image must serve the sandbox contract and their pods are routed by wildcard rather than a per-workload route:

```yaml
sandbox:
  enabled: true                     # the sandboxes service: serves /v1/sandbox and reconciles the pools
  domain: sandboxes.example.com     # needs a wildcard DNS record for *.sandboxes.example.com
  pools:
    - id: py
      image: python:3.12-slim         # any runtime image; the agent is installed
      port: 3000                      # where the agent listens
      size: 4
      runtimeClass: gvisor
      maxIdleSeconds: 900
  proxy:
    enabled: true
```

The **sandbox proxy** is the component every sandbox request passes through: one wildcard `HTTPRoute` for `*.{domain}` sends traffic to it, and it resolves the sandbox from the capability token in the request's `Host`.

It is deliberately its own component rather than a mode of the [deployments activator](deployments.md), because the two differ everywhere it matters: the proxy is permanently on the request path (so it scales with sandbox traffic, including file transfers, not with cold starts), it reads pods and nothing else — no Secrets, no scale writes — and it never raises anything, since a sandbox is a claimed pod. Separate components keep those blast radii, RBAC grants, and scaling knobs separate.

See [operations](operations.md#sandboxes) for the full deployment picture, and [observability](observability.md#pools) for the `kind="sandbox"` warm-pool series.

### The Docker backend

Sandboxes also run on the Docker development backend, so you can build against the API without a cluster. Each sandbox is a container running the pool's image, fronted by a sidecar, sharing a workspace volume; the sandbox proxy runs inside the sandboxes service itself and serves sandboxes on its own data port (`DATA_PORT`, default 8081), so URLs carry that port (`http://s-{token}.sandboxes.test:8081`). Running the deployments service on the same host? Give one of them a different `DATA_PORT` — each now has its own listener.

```yaml
# docker-compose / env for the sandboxes service
ORCHESTRATOR_BACKEND: docker
SANDBOX_DOMAIN: sandboxes.test   # the domain sandbox URLs are minted under
SANDBOX_POOLS_JSON: '[{"id":"py","image":"node:22-slim","port":3000,"maxIdleSeconds":900}]'   # optional warm capacity
DOCKER_NETWORK: orchestrator   # recommended: keeps sandboxes off the default bridge
```

Everything in this guide works there — the contract, artifacts, extra ports, idle teardown, `status`/`list` reconstruction after a restart — with two honest exceptions:

- **No warm pool.** `size` is ignored and `warm` is always `0`: a create pays a full container start (seconds), where Kubernetes claims an already-running pod in well under one.
- **No mounts.** The `mount` artifact needs a mount shared from the sidecar into the workload, which pod containers get through propagation on a shared volume and sibling Docker containers do not. A mount is a `400` here even on a pool with `mounts: true`.
- **No isolation tiers.** `gvisor` and `kata` are RuntimeClasses, which Docker has no equivalent of. A sandbox here has ordinary container isolation, so **do not run untrusted code on it** — that is what the Kubernetes backend and a gVisor pool are for.

One environment note: the service reaches sandboxes by container address, so it must run where those are routable — beside the daemon, or on a host whose engine routes to containers (OrbStack does, Docker Desktop does not). This is the same constraint the Docker deployments backend already has.

## Design notes

Not part of the API contract — the reasoning behind the shape above.

### Why the agent is installed rather than required of the image

The contract has to be served from inside the sandbox (see below), but requiring
it of the *image* would have made every pool image a custom build. So the agent
is installed instead: `ghcr.io/open-runtimes/sandbox` runs as an init container
with its command replaced by

```
cp /usr/local/bin/sandbox /workspace/.sandbox-agent
```

and the claim then execs that path. Plain `cp` — no shell, no mkdir — so the
publishing image needs to contain nothing but the binary and a `cp`, and the
destination sits at the workspace root for the same reason.

Distributing it as an image rather than a fetched artifact is the point: the tag
(or digest, via `sandbox.agentImage.ref`) IS the version pin, the registry
verifies the bytes, the kubelet caches it per node instead of per pod, and an
air-gapped install mirrors it like any other image. The alternative we tried —
vendoring the release tarball into our own image at build time — needed a fetch
script, hand-maintained SHA-256 digests, and a build-time network call, to end up
in the same place.

### Why exec and files live inside the sandbox

The alternatives are both worse. Through the control plane: `sandbox-service` is stateless with N replicas and is rolling-restarted on every deploy, so a long exec dies every time we ship; every tenant's file uploads share those replicas; and streaming stdout gains a hop of buffering. Through our own sidecar: the sidecar and worker are different containers sharing only the workspace `emptyDir`, not a process or mount namespace — so the sidecar cannot run the worker image's interpreter or see a worker-only volume mount. Fixing that means either a resident supervisor in `cmd/pool-shim` (it `syscall.Exec`s the payload as PID 1 and dies with it, so this is an inversion plus a socket protocol) or backend-native exec (`pods/exec` + Docker exec: two exec implementations, two file-copy implementations, and `pods/exec` RBAC we would rather not grant).

Putting the contract in the image costs none of that and buys bring-your-own-image. The cost is a contract to version and a reference image to maintain.

### Routing: wildcard, not per-sandbox routes

Activations get a Service and HTTPRoute each, which is fine at tens-to-hundreds of long-lived activations. It does not extend to sandboxes, for two reasons — and the second is disqualifying:

1. **Churn.** Thousands of sandboxes with minute-scale lifetimes means a gateway config recompute and xDS push per create and per delete.
2. **Programming latency.** A new HTTPRoute is not live until the gateway programs it, often seconds. We would return a URL that 503s for longer than the sub-second claim took — negating the entire reason to use a warm pool.

So sandboxes get one wildcard HTTPRoute backed by the sandbox proxy, which resolves the sandbox by Host. No per-sandbox Service, route, or endpoint flip; nothing to churn; creates as fast as the claim.

### The URL is a capability

With a wildcard route and no per-sandbox auth, **reaching a sandbox's URL is sufficient to execute code in it.** The URL is the credential, so it has to be unguessable — and the `id` cannot serve, because a caller-chosen `my-agent-sandbox` is guessable no matter how much entropy our generator has.

So the host carries its own 16-byte token, independent of the id:

```
POST /v1/sandbox  {"id": "my-agent", ...}
  → {"id": "my-agent", "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com"}

/v1/sandbox/my-agent                # stable, caller-chosen, idempotent
s-9f3c…95f.sandboxes.example.com    # unguessable, the capability
```

The token is also the routing key: warm pods are labelled with it on claim, so the proxy's lookup stays as cheap as keying on the id would have been. It lives only as that label — never in an annotation, a log line, or an event payload — so `DELETE` invalidates it along with the pod, and a leaked URL is dead on teardown.

One module renders that hostname and reads it back (`internal/sandbox`, `Addressing`), so the writer cannot drift from the reader, and the `s-` prefix is an enforced invariant rather than a convention: a host that merely shares the domain is not a sandbox and is not resolved as one.

Proxy authentication — a per-sandbox bearer token the proxy requires — remains the stronger answer and stays open. It decouples addressability from authorization outright, and the proxy is the right place for it since it is already the only thing on the path. Worth doing if sandboxes ever hold tenant data.

### Deliberately out of scope

**Suspend/resume.** It requires owning per-sandbox PVC lifecycle, quota, and cleanup, and a resume cannot use warm-pool claiming, so it needs a second, slower creation path. Ephemeral workspaces plus pool-level volumes cover the agent use case.

**Per-sandbox managed storage.** Same reasoning, plus the warm-pod constraint: pool `volumes` attach storage whose lifecycle someone else owns, which is exactly why they are cheap, and a live pod's mounts cannot be changed at claim time regardless. There is a cheaper shape that gets most of the way — restore by mounting a filesystem image at create, snapshot by archiving and uploading at teardown, with S3 as the store and no PVC lifecycle to own. See [sandbox mounts and shutdown artifacts](design/sandbox-mounts-and-shutdown-artifacts.md).

**A sandbox network policy.** Untrusted code with default-open egress is the real exposure. The workload-namespace policy blocks the cloud metadata endpoint and default-denies ingress, but a sandbox-specific default-deny egress is a platform concern spanning pools and deployments too — its own change, not this one.

**Callbacks.** A sandbox has no lifecycle event worth delivering asynchronously: creation is synchronous and sub-second, and what happens *inside* it is between you and the contract. [Jobs and deployments](callbacks.md) emit events because their work outlives the request that started it.
