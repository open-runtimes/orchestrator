# Sandbox mounts and shutdown artifacts

**Status: partly implemented.** Mounts on sandbox pools have shipped — see [Mounting a filesystem image](../sandboxes.md#mounting-a-filesystem-image) for what exists. Shutdown artifacts have not; the sections on them stand as the proposal. The [sandbox guide](../sandboxes.md) describes what exists today; this describes what it would take to support two things it currently refuses, and what I would want settled before writing either.

## What is being asked for

A **persistent folder** for a sandbox, without the orchestrator owning storage:

1. At create: `download` an erofs (or squashfs) image, `mount` it — read-only, or writable with a tmpfs overlay.
2. Work in it.
3. At teardown: `archive` the workspace and `upload` it to S3, so the next sandbox restores from it.

That is checkpoint/restore built out of artifacts, in userspace, with S3 as the store. It is a good shape: it needs no PVC lifecycle, no quota accounting, and no second creation path — the three things that put [suspend/resume](../sandboxes.md#deliberately-out-of-scope) out of scope in the first place.

Worth noting before anything else: **every artifact type this needs already exists.** `download`, `mount` (including `writable` with a tmpfs overlay and a `size` cap), `archive` with `format: "erofs"`, and `upload` to `s3://` are all implemented and used by jobs today. Nothing about the vocabulary is missing. What is missing is a pod that can perform a mount, and a phase that runs at teardown.

Two things block it. One is a real constraint. The other is mostly a phase we already have and never run.

## Why mounts are refused today

Not because a sandbox is the wrong shape for them. Because nothing in a sandbox pod owns a mount's lifecycle.

A mount is the one artifact whose work is not `Apply`'s. `internal/sidecar/mount_linux.go` associates the image with a loop device and calls `mount(2)` directly — no external binaries, so it works on a distroless image — which needs three things from the pod:

| Requirement | How jobs get it |
| --- | --- |
| `CAP_SYS_ADMIN` and loop-device access | the post sidecar runs `Privileged`, `RunAsUser: 0` — and only in pods whose job declares a mount |
| The mount visible in the worker's mount namespace | the shared volume is `Bidirectional` in the sidecar and `HostToContainer` in the worker |
| A resident process to establish it before the worker starts, hold it, and release it after | the post sidecar: establishes mounts, writes a marker the worker's startup probe gates on, unmounts when the worker exits |

A sandbox pod has none of that. Its sidecar runs a **pre phase only** — `RunPre` on the claim path (`internal/proxy/pool.go`) — with no post phase and no propagation, so a `mount` artifact had nowhere to be performed. Until recently it was accepted and silently dropped; it is now a `400`, which is honest but is not the same as impossible.

## What it would actually take

### 1. The resident owner already exists

The workload sidecar (`internal/proxy`) lives for the whole life of the sandbox and already runs artifacts on the claim path. It is a better owner than the job sidecar, because **the claim is already the barrier**: nothing runs in the workload container until the sidecar writes the FIFO line. So the sequence needs no marker file and no startup probe:

```
claim arrives
  → materialize pre-phase artifacts (the erofs image lands in the workspace)
  → establish mounts
  → signal the shim
  → the payload execs, and the mount is already there
```

The workload container is already running as the shim when the mount happens, so this leans on propagation being **dynamic** — a new mount under a `Bidirectional`/`HostToContainer` path appearing in an already-running container. That is how shared subtree propagation is supposed to behave, and jobs never exercise it (their worker starts after the mount). **It is the first thing the spike below should prove.**

### 2. Mount capability is a pool dimension

Privilege and propagation are properties of a pod spec, and a warm pod is created long before any claim. So the *capability* cannot be per-request:

```yaml
sandboxes:
  pools:
    - id: restore
      image: python:3.12-slim
      mounts: true      # this pool's sidecar runs privileged, with propagation
      runtimeClass: runc
```

The `mount` **artifact** stays per-sandbox; the pool declares whether its pods can perform one. That is the same split `volumes` and `runtimeClass` already have, and for the same reason. A `mount` artifact against a pool without the capability stays a `400`, with a message that names the pool setting.

### 3. Isolation is the real constraint

This is what I would want decided before any code.

Sandbox pools are exactly where we tell people to run [gVisor or Kata](../sandboxes.md#isolation), because untrusted model-generated code is the expected workload. Loop-mounting a filesystem image inside gVisor is not something to assume works — the sentry implements its own VFS and does not generally hand out block devices — and Kata is a VM with its own story. If mounts only work under `runc`, then this feature lands **precisely where isolation is weakest**, and it puts a privileged container in the same pod as untrusted code.

Jobs accept that trade because the privileged sidecar exists only in pods for jobs that asked to mount, running the operator's own workload. A sandbox pool is standing capacity for arbitrary callers' code, so "privileged sibling container" is a materially different proposition.

That does not kill it. It means the feature is honestly scoped as: **`mounts: true` requires `runtimeClass: runc`, is off by default, and the docs say plainly that a pool with both mounts and untrusted code is not a boundary you should rely on.** If the spike finds gVisor can do it, that caveat goes away and this gets much more attractive.

## Shutdown artifacts are nearly free

The phase already exists. An artifact that depends on the sentinel `"job"` is a post-phase artifact; `artifact.Partition` splits them; `sidecar.Runner.RunPost` runs them. Jobs use it for exactly this — archive and upload after the work finishes. Three things are missing for sandboxes:

1. **Nobody runs the post phase.** The workload sidecar already drains on `SIGTERM` (`cmd/workload-sidecar`), which is precisely the moment: fail readiness, let in-flight requests finish, *then* run post artifacts, then exit.
2. **The grace period is too short.** Warm pods do not set `terminationGracePeriodSeconds`, so they get Kubernetes' default 30s — not enough to archive and upload a workspace of any size. Job pods take it from config (600s by default). It becomes a pool dimension.
3. **`DELETE` is fire-and-forget.** It returns `204` and the pod goes. If a caller needs to know the snapshot landed — and for this use case they do, or the folder silently is not persisted — teardown needs to be observable.

### The sentinel needs a better name

`"depends": "job"` on a sandbox artifact reads wrong. The sentinel means "after the workload", not "after a job". I would add `"workload"` as the spelling, keep `"job"` as an accepted alias forever, and document one of them.

### The API shape to settle

Today `DELETE` → `204`, gone. With a finalize phase there is a window where the sandbox is not serving but is not finished:

```
DELETE /v1/sandbox/{id}   → 202 Accepted
GET    /v1/sandbox/{id}   → {"status": "finalizing"}
                          → 404 once the snapshot has landed and the pod is gone
```

That needs a decision I do not want to make unilaterally, because it changes an existing endpoint's contract:

- **Does `DELETE` stay `204` when a sandbox has no post-phase artifacts?** I would say yes — no finalization, nothing to observe, no reason to make every caller poll.
- **How does a caller learn the snapshot *failed*?** A `404` cannot say "the upload failed". Either the record survives briefly in a terminal `failed` state with the reason, or failures are only observable in logs and callbacks — and sandboxes deliberately [have no callbacks](../sandboxes.md#deliberately-out-of-scope). A terminal record with a TTL is the smallest honest answer.
- **Does an idle reap finalize too?** It has to, or the pattern silently loses data whenever a caller forgets to `DELETE` — which is the case the idle timeout exists for. Same for a pool being scaled down.

### The semantics have to be stated as best-effort

A shutdown artifact runs when the sidecar gets a `SIGTERM` and finishes within the grace period. It does **not** run on node loss, preemption, an OOM kill of the sidecar, or a grace-period overrun. Anyone who reads "upload on shutdown" as durable will eventually lose a session's work.

If that is not good enough for the use case, the answer is periodic snapshots — which the caller can already do today, from inside, with an `/execute` call on a timer.

### The reason to do this in the platform at all

The sandbox could run `aws s3 cp` itself today. The difference is credentials: the artifact runner **already holds the S3 credentials** (`sidecar.WithS3Credentials` on the claim path), and the untrusted workload never sees them. Doing the upload as an artifact keeps the secret on the sidecar's side of the container boundary. For untrusted code that is the whole argument, and it is a good one.

## The use case, end to end

Assuming both pieces land:

```yaml
sandboxes:
  pools:
    - id: agent
      image: python:3.12-slim
      mounts: true
      runtimeClass: runc                     # see the isolation caveat above
      terminationGracePeriodSeconds: 300     # bounds the snapshot
      maxIdleSeconds: 1800
```

```jsonc
POST /v1/sandbox
{
  "pool": "agent",
  "artifacts": [
    // restore: fetch last session's image and mount it writable, changes in tmpfs
    {"id": "img",   "type": "download", "in": "s3://acme/sessions/42.erofs", "out": "session.erofs"},
    {"id": "mount", "type": "mount", "in": "session.erofs", "out": "work",
     "writable": true, "size": 512, "depends": "img"},

    // snapshot: after the workload, archive the tree and put it back
    {"id": "snap",  "type": "archive", "in": "work", "out": "session.erofs",
     "format": "erofs", "depends": "workload"},
    {"id": "store", "type": "upload", "in": "session.erofs",
     "out": "s3://acme/sessions/42.erofs", "depends": "snap"}
  ]
}
```

One wrinkle worth knowing before building on it: `archive` over the mount point captures the **merged** overlay view, so a snapshot costs time proportional to the whole tree, not to what changed. The overlay's upper directory is where the changes actually are, and it is not exposed as a path today. Snapshotting only the delta would be a further change.

## Alternatives, and when they are better

| Instead of this | When it wins |
| --- | --- |
| `unarchive` rather than `mount` | Works on sandboxes **today**. Costs extraction time and roughly double the space, and the whole tree is read into the workspace. Fine below a few hundred MB; the wrong shape for a large read-mostly tree, which is exactly what mount is for. |
| Pool `volumes` (a PVC) | Durable with no snapshot step at all, and no privilege. But storage is shared across every sandbox in the pool and someone else owns its lifecycle — no per-session isolation. |
| Upload from inside via `/execute` | Works today, including on a timer. Needs credentials inside untrusted code, and has no shutdown hook. |
| Suspend/resume | Still out of scope, and this proposal is the reason: it gets most of the value without the orchestrator owning storage. |

## What I would do next

1. **Spike the mount question — half a day, and it gates the rest.** Two facts: does a loop mount made in a privileged sidecar propagate into an already-running sibling container, and does any of it work under gVisor and Kata. Nothing else should be designed until both answers are in.
2. **Ship shutdown artifacts on their own.** They need no privilege, no propagation, and no isolation caveat, and combined with `unarchive` they already deliver the whole restore/snapshot pattern for small-to-medium trees. The API decisions above are the real work.
3. **Then mounts, scoped to `runc` pools**, with the caveat documented, and revisited if the spike says gVisor can do it.

Splitting them that way means the half with an unresolved security question does not hold up the half without one.
