# Sandboxes Guide

> **Status: proposed, not implemented.** No `/v1/sandbox` endpoint exists yet. This is written as a guide because the API shape is the thing under review — the reasoning and the build delta are in [design notes](#design-notes) at the end. Nothing here is a contract until it ships.

A **sandbox** is a live, isolated workspace you drive from the outside: create one, run commands in it, read and write its files, tear it down. Where a [job](jobs.md) runs to completion and a [deployment](deployments.md) serves traffic under a stable name, a sandbox does neither — it sits there and waits for you. It is the shape an agent, a notebook kernel, or an interactive build wants.

A sandbox is created from a **sandbox pool** — standing warm capacity, the same claim-and-late-bind machinery [pools](pools.md) use — so creation is sub-second rather than a cold container start.

- [Creating a sandbox](#creating-a-sandbox)
- [The sandbox contract](#the-sandbox-contract)
- [Running commands](#running-commands)
- [Files](#files)
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

**Treat the URL as a secret.** Anyone who can reach it can run commands in the sandbox, so its hostname is an unguessable token rather than your `id` — don't log it, and don't hand it to anyone you wouldn't hand a shell. `DELETE` invalidates it.

`command` is optional and defaults to the pool's: a sandbox pool's image already serves the sandbox contract, so there is usually nothing to late-bind but artifacts.

## The sandbox contract

**Exec and files are not part of this API.** They are an HTTP contract the sandbox *image* implements, and you reach them at the sandbox's own URL. The orchestrator creates, routes, and reaps sandboxes; it does not sit in the middle of your commands.

`open-runtimes/sandbox` is the reference image. Any image that answers these three routes on the pool's port is a valid sandbox image:

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

For the reference image: commands run in the worker container — same image, same filesystem, same [isolation tier](#isolation) — with the workspace as the working directory, and are **serialized per sandbox** (a second `/execute` against a busy sandbox is `409`). One sandbox is one seat; concurrent execs would race on a shared filesystem. Output is capped at 1 MiB per stream, past which `truncated` is `true` — beyond that, write to a file and read it back.

## Files

```bash
curl -X PUT  http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/files/main.py --data-binary @main.py
curl         http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com/files/out.json
```

Paths are relative to the workspace; `..` and absolute paths are `400`. `GET` on a directory lists it as JSON.

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

If artifact materialization fails the sandbox is `failed` with the reason, and its pod is **poisoned** — discarded and replaced, never handed to another sandbox.

## Persistence

The workspace is ephemeral: an `emptyDir` that dies with the sandbox. Two opt-ins buy durability:

- **`artifacts`** — bulk in, at create time (above).
- **`volumes`** — mount an existing Docker volume or K8s PVC, the same [schema](jobs.md#volumes) jobs and deployments use. Attach-only: the orchestrator never creates, sizes, or deletes the storage.

```json
{"pool": "py", "volumes": [{"source": "agent-scratch", "path": "/data"}]}
```

Nothing is checkpointed and there is no suspend/resume — a sandbox is either running or gone. Anything worth keeping goes in a volume, or gets read out through `/files` before teardown.

## Isolation

A sandbox runs at its pool's isolation tier, set by the pool's `runtimeClass` (`runc` | `gvisor` | `kata`). Warm pods are runtime-fixed at creation, so this is a **pool dimension, not a per-sandbox field** — warm fleets are keyed by `(image, runtimeClass)`. Want gVisor-isolated sandboxes? Configure a gVisor pool and create from it.

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

`idleTimeoutSeconds` tears the sandbox down after that long with no traffic (`0` = live until `DELETE`). Pools should set a ceiling: an abandoned sandbox holds a warm pod hostage.

As with activations, a used pod is **never reused** — teardown discards it and the pool replenishes with a fresh one.

## Sandbox pools

Sandbox pools are **operator configuration**, not an API resource: adding, resizing, or removing one is a config change and a rollout. The API over them is read-only.

```bash
curl http://localhost:8080/v1/sandbox-pool
```

```json
{"pools": [{"id": "py", "image": "open-runtimes/sandbox-python:3.12", "runtimeClass": "gvisor", "size": 4, "warm": 4, "claimed": 1}]}
```

`GET /v1/sandbox-pool/{id}` returns one pool. They are configured exactly like [deployment pools](operations.md#pools) — `size`, `cpu`, `memory`, `runtimeClass`, `burst`, `volumes`, `port` — and are a separate fleet because their image must serve the sandbox contract and their pods are routed by wildcard rather than per-workload route.

Sandboxes require the Kubernetes backend; the Docker development backend serves deployments and jobs only.

---

## Design notes

Not part of the API contract — the reasoning behind the shape above, and the delta to build it.

### The shared engine

The claim protocol is the whole point, and it is currently misfiled. `internal/pool/claim` is written against an `Inventory` seam and describes itself in backend-neutral terms, but it lives under `internal/pool` as though it belonged to one consumer. It has at least three: activations, sandboxes, and deployment cold starts.

The shared concept is **warm fleet + claim + late-bind**, and it gets a neutral home before sandboxes are built on it — not after:

| Today | Concept | Home |
| --- | --- | --- |
| `internal/pool/claim` | the claim protocol: POST-is-the-claim, poison, burst fallback, steal-retry | `internal/claim` |
| `internal/pool/kubernetes` (inventory + replenish) | warm fleet reconcile and replenishment | `internal/warm` |
| `pkg/pool.Pool` | a fleet declaration: `id`, `image`, `runtimeClass`, `size`, `cpu`, `memory`, `burst`, `volumes` | `pkg/fleet.Fleet`, specialized per consumer |
| the artifact round-trip (`Parse`/`UnmarshalJSON`/`MarshalJSON`) in `pkg/pool/types.go:100-178` **and** `pkg/deployment/types.go:127+` | artifact-bearing spec codec | one generic helper in `internal/artifact` |

`pkg/lifecycle` is already correctly neutral — it is the model to follow.

The last row lands first, independently: it is already duplicated twice today, so it pays for itself with no sandbox code written, and a third copy would be indefensible. The `internal/pool/claim` → `internal/claim` move is mechanical (only the package path is wrong).

This extraction is the load-bearing part of the design. One claim-and-late-bind engine serving jobs, deployments, activations, and sandboxes across Docker and Kubernetes is the thing worth having; a second copy of the claim flow sitting next to the first is a worse `agent-sandbox`.

New code on top: `pkg/sandbox/{types,service,orchestrator}.go`, `internal/api/sandboxes.go`, a sandbox `Inventory`, and wildcard resolution in the activator. Notably *not* new: the shim, the sidecar, the claim protocol, the artifact pipeline, `pkg/volume`.

### Why exec and files live in the image

The alternative was serving them ourselves, and both ways of doing that are worse.

Through the control plane: `deployments-service` is stateless with N replicas and is rolling-restarted on every deploy, so a long exec dies every time we ship; every tenant's file uploads share those replicas; and streaming stdout gains a hop of buffering. Through our own sidecar: the sidecar and worker are different containers sharing only the workspace `emptyDir`, not a process or mount namespace — so the sidecar cannot run the worker image's interpreter or see a worker-only volume mount. Fixing that means either a resident supervisor in `cmd/pool-shim` (it currently `syscall.Exec`s the payload as PID 1 at `cmd/pool-shim/main.go:128` and dies with it, so this is an inversion, ~200 lines plus a socket protocol) or backend-native exec (`pods/exec` + Docker exec: two exec implementations, two file-copy implementations, and `pods/exec` RBAC we would rather not grant).

Putting the contract in the image costs none of that and buys bring-your-own-image. It is also what `kubernetes-sigs/agent-sandbox` converged on, though somewhat by accident: their SDK `POST`s `/execute` to a server that ships in the sandbox image (`clients/python/.../commands/command_executor.py`, `examples/demo-cilium-egress/exec-sandbox/server.py`), invisible to the CRD, the controller, and its RBAC.

The cost is a contract we must version and a reference image we must maintain. Serializing exec per sandbox is deliberate and belongs in the contract: it keeps the reference implementation single-threaded and sidesteps concurrent-write races. Relaxing it later is compatible; the reverse is not.

### Routing: wildcard, not per-sandbox routes

Deployments get an HTTPRoute per deployment with stable `backendRefs`, and `endpointflip` swaps the revision Service's endpoints between ready pods (warm) and the shared activator (cold or draining) — so the activator buffers cold starts without sitting on the warm path (`internal/deployment/endpointflip/reconciler.go:1-5`). Activations extend that with a Service and HTTPRoute per activation (`internal/pool/kubernetes/route.go:35,62`), which is fine at tens-to-hundreds of long-lived activations.

It does not extend to sandboxes, for two reasons — and the second is the disqualifying one:

1. **Churn.** Thousands of sandboxes with minute-scale lifetimes means a Gateway config recompute and xDS push per create and per delete.
2. **Programming latency.** A new HTTPRoute is not live until the gateway programs it, often seconds. We would return a URL that 503s for longer than the sub-second claim took — negating the entire reason to use a warm pool.

So sandboxes get **one wildcard HTTPRoute** for `*.{sandbox domain}`, backed by the activator, which resolves the sandbox by Host — it already routes by Host (`internal/activator/activator.go:33`) and already owns the `Prefer: respond-async` split. No per-sandbox Service, route, or flip slice; nothing to churn; creates as fast as the claim. The wildcard-DNS half of this is already how deployments are reached (`docs/operations.md:112`).

This is the same topology as agent-sandbox's Sandbox Router — a shared edge keyed by sandbox identity — but reusing an edge we already own and keying on Host rather than a bespoke `X-Sandbox-ID` header. Their router is inline permanently because it is the only thing that can resolve that header; ours can step aside per-sandbox via the deployments route-and-flip path when a sandbox declares `hosts` and wants gateway-direct serving.

### How the sandbox edge fits the activator

`internal/activator` already hosts **two** edges over one broker, which settles the question of whether a third fits: `Activator` routes by Host through a `Resolver` (Docker/Phase 1, `activator.go:88`), and `RevisionActivator` routes by the gateway's `X-Revision` header through pod informers (`revision.go:130`). The `Resolver` seam belongs to the first edge only — a sandbox edge would not touch it.

What the broker actually requires is small: an opaque `key string` and a `capacity` with `Target(ctx) (*url.URL, error)` and `Raise(ctx) error` (`broker.go:40-46`). A `SandboxActivator` supplies both trivially:

- **Host → id needs no lookup.** With `{id}.{sandbox domain}`, the leading DNS label *is* the sandbox id. Cheaper than either existing edge — no `Resolver` scan, no informer read, no resolve cache (`activator.go:67-84` becomes unnecessary).
- **`Target`** is `revisionCapacity.Target` with a different label selector: list pods for the sandbox id, take a ready one. `readyPodTarget`, `podDataTarget`, `probeCandidates`, and `probeReady` (`revision.go:197-266`) are reusable as-is — including the direct-sidecar-probe trick that beats kubelet readiness propagation, which matters for a sub-second claim.
- **`Raise` is a no-op.** A sandbox has no scale-from-zero: it is a claimed pod, and if the pod is gone the sandbox is gone. This dissolves the "does being on the data path conflict with scale-from-zero" worry — there is no raise to conflict with. Holding still earns its keep during `creating` (the pod exists, artifacts are materializing), so `hold` should be a few seconds rather than the deployments `StartTimeout` of 300s.

**Use `broker.sync` only.** The async path is deployment-typed — `async` takes a `*deployment.Request` for `spec.Callback`/`spec.TimeoutSeconds`, and `dispatchResponse` hardcodes the `orchestrator.deployment.response` event type, the `deploymentId` key, and `source = "orchestrator/deployments"` (`broker.go:121,199,247`). Generalizing that means parameterizing the callback/event triple, and there is no reason to: async exec belongs to the image's contract now, not ours. Sync-only also keeps file uploads streaming through `httputil.ReverseProxy` (`broker.go:326`) rather than hitting the async path's 10 MiB buffer.

**Deploy it as its own Deployment.** The sandbox edge is permanently on the data path, while `deployments-activator` only sees cold and async traffic — different load profiles, and sandbox file transfers should not share a failure domain with deployment cold starts. Same binary and image, separate replica set and selector.

The bookkeeping maps are already churn-safe (`pruneMapThreshold = 1024`, `pruneStale`, `broker.go:400-416`), which matters at sandbox create/delete rates. One wart: the `Recorder` metrics are activator-generic in name but would conflate sandbox and deployment holds in one series — wants a label or a second recorder.

### The URL is a capability

With a wildcard route and no per-sandbox auth, **reaching a sandbox's URL is sufficient to execute code in it.** The URL is the credential, so it has to be unguessable.

Activation ids are minted from 4 random bytes (`pkg/pool/service.go:159-169`) — 32 bits behind a predictable `{pool}-` prefix. Fine for activations, where URL secrecy was never the security model; not fine when a guess yields arbitrary code execution inside someone else's gVisor sandbox.

**The host carries its own 16-byte token, independent of the sandbox id.** This is not just a wider id, because `id` is caller-choosable for idempotency and stable API paths — and a caller-chosen `my-agent-sandbox` is guessable no matter how much entropy our generator has. Decoupling keeps both properties:

```
POST /v1/sandbox  {"id": "my-agent", ...}
  → {"id": "my-agent", "url": "http://s-9f3c1a04b7e28d65f1024c8ba3e7d95f.sandboxes.example.com"}

/v1/sandbox/my-agent          # stable, caller-chosen, idempotent
s-9f3c…95f.sandboxes.example.com  # unguessable, the capability
```

The token is the routing key: warm pods are labelled with it on claim, so the edge's `Target` selector keys on the token and the lookup stays as cheap as keying on the id would have been. The host is no longer derivable from the id, so it must be returned at create time and available from `GET /v1/sandbox/{id}` — which it would be anyway.

Consequences to honor: the URL is a secret, so it stays out of logs, error bodies, redirects, and CloudEvent payloads. `DELETE` must invalidate the token, not just the pod, so a leaked URL is dead on teardown.

Edge authentication — a per-sandbox bearer token the `SandboxActivator` requires — remains the stronger answer and stays open. It decouples addressability from authorization outright, and the edge is the right place for it since it is already the only thing on the path. Worth doing if sandboxes ever hold tenant data. `agent-sandbox` punts here too: their router's default authorizer is `AllowAll`, with a real one merely recommended in their threat model.

### The `sandbox` → `runtimeClass` rename

`sandbox` currently names the isolation tier on jobs, deployments, and pools. Once sandbox is also a workload kind, one word means two things. Kubernetes calls the resource `RuntimeClass` and the field `spec.runtimeClassName`; plain `runtime` is unavailable because open-runtimes already uses it for the language runtime image. So the tier becomes `runtimeClass`, and `sandbox` is freed for the kind.

Breaking, and cheap now:

| | from | to |
| --- | --- | --- |
| API field | `"sandbox": "gvisor"` | `"runtimeClass": "gvisor"` |
| Constants | `SandboxRunc/Gvisor/Kata`, `ValidSandbox` | `RuntimeClassRunc/Gvisor/Kata`, `ValidRuntimeClass` |
| Env | `KUBE_SANDBOX_RUNTIME_CLASSES` | `KUBE_RUNTIME_CLASSES` |
| File | `internal/kube/sandbox.go` | `internal/kube/runtimeclass.go` |

Call sites: `pkg/deployment/types.go:17,72-83`, `pkg/pool/types.go:22,75-78`, `internal/kube/sandbox.go`, `internal/deployment/{docker,kubernetes}`, `internal/pool/kubernetes`, `charts/orchestrator`, `docs/{deployments,operations}.md`, `UBIQUITOUS_LANGUAGE.md:139`. Mechanical, and it deletes the ambiguity rather than documenting around it.

### Deliberately out of scope

**Suspend/resume.** `agent-sandbox` makes this first-class (`operatingMode: Running|Suspended` — delete the pod, keep the CR, Service, and PVCs). Right feature eventually, wrong one now: it requires owning per-sandbox PVC lifecycle, quota, and cleanup, and a resume cannot use warm-pool claiming, so it needs a second, slower creation path. Ephemeral-plus-attachable-volumes covers the agent use case.

**Per-sandbox managed storage.** Same reasoning — `volumes` attaches storage whose lifecycle someone else owns, which is exactly why it is cheap.

**A sandbox network policy.** Untrusted code with default-open egress is the real exposure, and we have no equivalent to agent-sandbox's managed default-deny NetworkPolicy (`docs/security/threat_model.md`). A genuine gap, but a platform concern spanning pools and deployments too — its own change, not this one.
