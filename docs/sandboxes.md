# Sandboxes Guide

A **sandbox** is a live, isolated workspace you drive from the outside: create one, run commands in it, read and write its files, tear it down. Where a [job](jobs.md) runs to completion and a [deployment](deployments.md) serves traffic under a stable name, a sandbox does neither — it sits there and waits for you. It is the shape an agent, a notebook kernel, or an interactive build wants.

A sandbox is created from a **sandbox pool** — standing warm capacity, the same claim-and-late-bind machinery [pools](pools.md) use — so creation is sub-second rather than a cold container start.

- [Creating a sandbox](#creating-a-sandbox)
- [The sandbox contract](#the-sandbox-contract)
- [Running commands](#running-commands)
- [Files](#files)
- [Ports](#ports)
- [Persistence](#persistence)
- [Isolation](#isolation)
- [Lifecycle](#lifecycle)
- [Sandbox pools](#sandbox-pools)

## Creating a sandbox

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

`201 Created`, and the URL is live — no per-sandbox gateway programming to wait on. The `id` is optional; pass one for a stable API path and idempotency (re-POSTing an existing id is `409`).

**Treat the URL as a secret.** Anyone who can reach it can run commands in the sandbox, so its hostname is an unguessable 128-bit token rather than your `id` — don't log it, and don't hand it to anyone you wouldn't hand a shell. `DELETE` invalidates it.

`command` is optional and defaults to the pool's: a sandbox pool's image already serves the sandbox contract, so there is usually nothing to late-bind but artifacts. `timeoutSeconds` bounds each request to the sandbox's URL — omitted takes 300, the maximum is 3600, and an explicit `0` means no bound at all (see [ports](#ports) for when you want that).

## The sandbox contract

**Exec and files are not part of this API.** They are an HTTP contract the sandbox *image* implements, and you reach them at the sandbox's own URL. The orchestrator creates, routes, and reaps sandboxes; it does not sit in the middle of your commands.

[`open-runtimes/sandbox`](https://github.com/open-runtimes/sandbox) is the reference image (`ghcr.io/open-runtimes/sandbox`). Any image that answers these three routes on the pool's port is a valid sandbox image:

| | |
| --- | --- |
| `POST /execute` | `{"command", "timeoutSeconds"}` → `{"exitCode", "stdout", "stderr", "durationMillis", "truncated"}` |
| `GET\|PUT\|DELETE /files/{path}` | whole-file read / write / remove, relative to the workspace |
| `GET /healthz` | readiness — the pool's probe subject |

That split is deliberate. It keeps the control plane off the data path, so a long exec is not killed by an orchestrator rolling restart and a large file upload is not a shared-fate bottleneck. It also means you can bring your own sandbox image — a computer-use image, a JVM with a warm classloader, an image that speaks a protocol we have never heard of — without changing anything here.

## Running commands

```bash
curl -X POST http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/execute \
  -H "Content-Type: application/json" \
  -d '{"command": "python -c \"print(2+2)\"", "timeoutSeconds": 30}'
```

```json
{"exitCode": 0, "stdout": "4\n", "stderr": "", "durationMillis": 41}
```

For the reference image: commands run in the worker container — same image, same filesystem, same [isolation tier](#isolation) — with the workspace as the working directory. Output is capped at 1 MiB per stream, past which `truncated` is `true`; beyond that, write to a file and read it back.

## Files

```bash
curl -X PUT  http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/files/main.py --data-binary @main.py
curl         http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/files/out.json
```

Paths are relative to the workspace; `..` and absolute paths are `400`. `GET` on a directory lists it as JSON — including the pool machinery's own `.pool/`, `.pool-exec.fifo`, and `.pool-shim.log`, which share the workspace volume. Ignore them; they are inert once the sandbox is serving.

For anything bulkier, use [artifacts](jobs.md#artifacts) at create time — the bulk-in path, materialized into the workspace by the sidecar before the sandbox reports ready:

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

If artifact materialization fails the sandbox is `failed` with the reason and no URL, and its pod is **poisoned** — discarded and replaced, never handed to another sandbox.

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

- **Only declared ports are reachable.** The port travels in the hostname; the edge turns it into a hint the sidecar checks against the claim, and the dial happens on loopback inside that sandbox's own pod. A port you did not declare is `404`, and a hint a client sets by hand is discarded.
- **The port shares the token's DNS label** (`s-{token}-5173`), rather than nesting as `s-5173.{token}`. A wildcard certificate covers exactly one label, so the flat form is reachable under one `*.{domain}` cert while the nested form would need a certificate per sandbox.

Readiness is the primary port's alone: a secondary port that never comes up does not fail the sandbox, and traffic to it counts as activity for the [idle timeout](#lifecycle) like any other request. `8000` and `8001` belong to the sidecar and are refused.

WebSocket traffic (terminals, LSP) upgrades cleanly through the edge. Create those sandboxes with `"timeoutSeconds": 0` — the per-request bound applies to an upgraded connection like any other request, so at the default it would cut the session after five minutes. `0` removes the bound for that sandbox; artifact materialization keeps its own budget regardless, so an unbounded sandbox is not an unbounded download.

## Persistence

The workspace is ephemeral: an `emptyDir` that dies with the sandbox. Two opt-ins buy durability:

- **`artifacts`** — bulk in, per sandbox, at create time (above).
- **`volumes`** — an existing K8s PVC, the same [schema](jobs.md#volumes) jobs and deployments use. Attach-only: the orchestrator never creates, sizes, or deletes the storage.

**`volumes` is a pool dimension, not a per-sandbox field** — the same constraint as [`runtimeClass`](#isolation), and for the same reason. A warm pod is already running when you claim it, and its mounts were fixed when it was created; the claim protocol late-binds a command, environment, and artifacts, but it cannot attach storage to a live pod. So the volume is declared on the pool and mounted into every warm pod in that fleet:

```yaml
# values.yaml — operator config, not an API call
deployments:
  sandboxes:
    pools:
      - id: py
        image: ghcr.io/open-runtimes/sandbox:0.1.0
        volumes:
          - source: agent-scratch
            path: /data
```

Want per-sandbox storage? Declare a pool per storage shape. Accepting `volumes` on the create call would mean cold-starting a pod for it, which silently turns a sub-second create into a slow one — a bad thing to do quietly.

Nothing is checkpointed and there is no suspend/resume — a sandbox is either running or gone. Anything worth keeping goes in a pool volume, or gets read out through `/files` before teardown.

## Isolation

A sandbox runs at its pool's isolation tier, set by the pool's `runtimeClass` (`runc` | `gvisor` | `kata`). Warm pods are runtime-fixed at creation, so this is a **pool dimension, not a per-sandbox field** — warm pools are keyed by `(image, runtimeClass)`. Want gVisor-isolated sandboxes? Configure a gVisor pool and create from it.

Untrusted, model-generated code is the expected workload here, so `gvisor` or `kata` is the right default for a sandbox pool even though `runc` is the platform default. See [operations](operations.md#isolation-tiers) for mapping tiers to your cluster's RuntimeClasses.

## Lifecycle

| `status` | Meaning |
| --- | --- |
| `creating` | Claimed; artifacts materializing |
| `ready` | Contract served at its URL |
| `failed` | Artifacts failed, or the image never became ready |
| `deleting` | Teardown in progress |

```
GET    /v1/sandbox              # list live sandboxes
GET    /v1/sandbox/{id}         # one sandbox's status
DELETE /v1/sandbox/{id}         # tear down → 204
```

`idleTimeoutSeconds` tears the sandbox down after that long with no traffic (`0` = live until `DELETE`, where the pool allows it). A pool's `maxIdleSeconds` caps it and fills it in when omitted: an abandoned sandbox holds a warm pod hostage, so requesting more than the ceiling is a `400`.

As with activations, a used pod is **never reused** — teardown discards it and the pool replenishes with a fresh one. Nothing about a sandbox is held in service memory, so a restart of the control plane leaves live sandboxes serving and reconstructs their state by listing pods.

## Sandbox pools

Sandbox pools are **operator configuration**, not an API resource: adding, resizing, or removing one is a config change and a rollout. The API over them is read-only.

```bash
curl http://localhost:8080/v1/sandbox-pool
```

```json
{"pools": [{"id": "py", "image": "ghcr.io/open-runtimes/sandbox:0.1.0", "size": 4, "warm": 4, "claimed": 1}]}
```

`GET /v1/sandbox-pool/{id}` returns one pool. On Docker, `warm` is always `0` — see [the Docker backend](#the-docker-backend). They are configured exactly like [deployment pools](operations.md#pools) — `size`, `cpu`, `memory`, `runtimeClass`, `burst`, `volumes`, `port` — plus `command` (the image's entrypoint, which the claim execs) and `maxIdleSeconds`. They are a separate fleet because their image must serve the sandbox contract and their pods are routed by wildcard rather than a per-workload route:

```yaml
deployments:
  sandboxes:
    domain: sandboxes.example.com   # needs a wildcard DNS record for *.sandboxes.example.com
    pools:
      - id: py
        image: ghcr.io/open-runtimes/sandbox:0.1.0
        command: /usr/local/bin/sandbox
        port: 3000
        size: 4
        runtimeClass: gvisor
        maxIdleSeconds: 900
    edge:
      enabled: true
```

The **sandbox edge** is the component every sandbox request passes through: one wildcard `HTTPRoute` for `*.{domain}` sends traffic to it, and it resolves the sandbox from the capability token in the request's `Host`. It runs the deployments-activator image in `EDGE_MODE=sandbox` with its own replica set — permanently on the data path, unlike the deployments activator, so sandbox file transfers do not share a failure domain with deployment cold starts.

### The Docker backend

Sandboxes also run on the Docker development backend, so you can build against the API without a cluster. Each sandbox is a container running the pool's image, fronted by a sidecar, sharing a workspace volume; the edge runs in the deployments service itself and serves sandboxes on its data port, so URLs carry that port (`http://s-{token}.sandboxes.test:8081`).

```yaml
# docker-compose / env for the deployments service
ORCHESTRATOR_BACKEND: docker
SANDBOX_DOMAIN: sandboxes.test
SANDBOX_POOLS_JSON: '[{"id":"py","image":"ghcr.io/open-runtimes/sandbox:0.1.0","command":"/usr/local/bin/sandbox","port":3000,"maxIdleSeconds":900}]'
DOCKER_NETWORK: orchestrator   # recommended: keeps sandboxes off the default bridge
```

Everything in this guide works there — the contract, artifacts, extra ports, idle teardown, `status`/`list` reconstruction after a restart — with two honest exceptions:

- **No warm pool.** `size` is ignored and `warm` is always `0`: a create pays a full container start (seconds), where Kubernetes claims an already-running pod in well under one.
- **No isolation tiers.** `gvisor` and `kata` are RuntimeClasses, which Docker has no equivalent of. A sandbox here has ordinary container isolation, so **do not run untrusted code on it** — that is what the Kubernetes backend and a gVisor pool are for.

One environment note: the service reaches sandboxes by container address, so it must run where those are routable — beside the daemon, or on a host whose engine routes to containers (OrbStack does, Docker Desktop does not). This is the same constraint the Docker deployments backend already has.

## Design notes

Not part of the API contract — the reasoning behind the shape above.

### Why exec and files live in the image

The alternatives are both worse. Through the control plane: `deployments-service` is stateless with N replicas and is rolling-restarted on every deploy, so a long exec dies every time we ship; every tenant's file uploads share those replicas; and streaming stdout gains a hop of buffering. Through our own sidecar: the sidecar and worker are different containers sharing only the workspace `emptyDir`, not a process or mount namespace — so the sidecar cannot run the worker image's interpreter or see a worker-only volume mount. Fixing that means either a resident supervisor in `cmd/pool-shim` (it `syscall.Exec`s the payload as PID 1 and dies with it, so this is an inversion plus a socket protocol) or backend-native exec (`pods/exec` + Docker exec: two exec implementations, two file-copy implementations, and `pods/exec` RBAC we would rather not grant).

Putting the contract in the image costs none of that and buys bring-your-own-image. The cost is a contract to version and a reference image to maintain.

### Routing: wildcard, not per-sandbox routes

Activations get a Service and HTTPRoute each, which is fine at tens-to-hundreds of long-lived activations. It does not extend to sandboxes, for two reasons — and the second is disqualifying:

1. **Churn.** Thousands of sandboxes with minute-scale lifetimes means a gateway config recompute and xDS push per create and per delete.
2. **Programming latency.** A new HTTPRoute is not live until the gateway programs it, often seconds. We would return a URL that 503s for longer than the sub-second claim took — negating the entire reason to use a warm pool.

So sandboxes get one wildcard HTTPRoute backed by the sandbox edge, which resolves the sandbox by Host. No per-sandbox Service, route, or endpoint flip; nothing to churn; creates as fast as the claim.

### The URL is a capability

With a wildcard route and no per-sandbox auth, **reaching a sandbox's URL is sufficient to execute code in it.** The URL is the credential, so it has to be unguessable — and the `id` cannot serve, because a caller-chosen `my-agent-sandbox` is guessable no matter how much entropy our generator has.

So the host carries its own 16-byte token, independent of the id:

```
POST /v1/sandbox  {"id": "my-agent", ...}
  → {"id": "my-agent", "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com"}

/v1/sandbox/my-agent                # stable, caller-chosen, idempotent
s-9f3c…95f.sandboxes.example.com    # unguessable, the capability
```

The token is also the routing key: warm pods are labelled with it on claim, so the edge's lookup stays as cheap as keying on the id would have been. It lives only as that label — never in an annotation, a log line, or an event payload — so `DELETE` invalidates it along with the pod, and a leaked URL is dead on teardown.

Edge authentication — a per-sandbox bearer token the edge requires — remains the stronger answer and stays open. It decouples addressability from authorization outright, and the edge is the right place for it since it is already the only thing on the path. Worth doing if sandboxes ever hold tenant data.

### Deliberately out of scope

**Suspend/resume.** It requires owning per-sandbox PVC lifecycle, quota, and cleanup, and a resume cannot use warm-pool claiming, so it needs a second, slower creation path. Ephemeral workspaces plus pool-level volumes cover the agent use case.

**Per-sandbox managed storage.** Same reasoning, plus the warm-pod constraint: pool `volumes` attach storage whose lifecycle someone else owns, which is exactly why they are cheap, and a live pod's mounts cannot be changed at claim time regardless.

**A sandbox network policy.** Untrusted code with default-open egress is the real exposure. The workload-namespace policy blocks the cloud metadata endpoint and default-denies ingress, but a sandbox-specific default-deny egress is a platform concern spanning pools and deployments too — its own change, not this one.
