# Sandbox mounts and shutdown artifacts

**Status: mostly implemented.** Mounts have shipped for all four workload kinds,
and a writable mount can now `sync` its delta continuously — which replaced the
teardown trigger this document opened with rather than fixing it. What is left is
the explicit snapshot endpoint. Mounts have shipped for all four workload kinds — see [Mounting a filesystem image](../sandboxes.md#mounting-a-filesystem-image) for what exists. Shutdown artifacts have not; the sections on them stand as the proposal. The [sandbox guide](../sandboxes.md) describes what exists today; this describes what it would take to support two things it currently refuses, and what I would want settled before writing either.

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
sandbox:
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
sandbox:
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

## The opt-in durable workspace

The trigger problem above is not a bug to fix in place. It is what you get for
asking a dying pod to do work. Two measurements on kind decide the alternative:

```
pod Succeeded, object NOT deleted
  /var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~empty-dir/       still there
  .../kubernetes.io~empty-dir/work/data.txt                          GONE
```

An `emptyDir` is reaped when the pod **terminates**, not when its object is
deleted — so there is no window for anything to read it afterwards, and keeping
the pod object alive does not create one. Reaching in would need a `hostPath`
onto `/var/lib/kubelet`, which can read every pod's volumes and secrets on that
node. That is not a trade worth making for a snapshot.

A PersistentVolume does survive, and a node-local one solves the scheduling
question by itself:

```
ws-writer     Succeeded  node=orchestrator-dev-control-plane   wrote, then deleted
ws-finalizer  Succeeded  node=orchestrator-dev-control-plane   bound the same claim
  finalizer sees: session-data

pvc-68ca9af7…  class=standard  affinity=orchestrator-dev-control-plane
```

The PV carries `nodeAffinity`, so the scheduler *must* place the follow-up pod
on the node holding the data. Nothing pins it by hand. With an RWX volume the
node constraint disappears entirely, at the cost of an infra dependency and
slower I/O than local disk.

So: a sandbox may opt into a workspace that outlives it, and teardown becomes an
ordinary Job that mounts what is left.

### The surface

```jsonc
POST /v1/sandbox
{
  "image": "python:3.12-slim",
  "port": 3000,
  "workspace": {
    "size": "2Gi",                  // required; capped by the operator
    "storageClass": "local-path",   // optional; the operator's default otherwise
    "claim": "sbx-agent-42-workspace", // optional: bind an existing one — the restore
    "keep": false                   // optional: leave it behind when the sandbox goes
  },
  "artifacts": [
    {"id": "snap",  "type": "archive", "in": ".", "out": "/tmp/s.erofs",
     "format": "erofs", "depends": "workload"},
    {"id": "store", "type": "upload", "in": "/tmp/s.erofs",
     "out": "s3://acme/sessions/42.erofs", "depends": "snap"}
  ]
}
```

**Poolless only.** A warm pod's workspace is created with the pod, long before
any claim, so a pool-backed durable workspace would either be shared across
claims — one sandbox's data arriving in the next — or a PVC per warm pod sitting
idle waiting for one. Same rule as `volumes` and `runtimeClass`, same reason,
and here it is a correctness argument rather than a cost one.

### The PVC is the record

Today a sandbox's state is derived entirely from its pod: the pod IS the record.
That is what makes finalization hard, because the record is the thing being
destroyed. With a durable workspace the PVC outlives the pod, so it becomes the
record — carrying the spec and the teardown intent as annotations, exactly as the
pod carries them now.

That ordering is what makes this survivable:

1. **Create** — PVC first (labelled with the sandbox id, annotated with the
   spec), then the pod that mounts it.
2. **Delete** (or an idle reap) — annotate the PVC `finalize=pending`, **then**
   delete the pod, then answer `202`. If the replica handling the request dies
   between those steps, the intent is already durable.
3. **The control loop** — the same leader-elected loop that reaps idle
   sandboxes — sees a PVC with `finalize=pending` and no pod, and creates the
   finalizer.
4. **Success** → delete the PVC (unless `keep`), and the sandbox `404`s.
   **Failure** → the sandbox reports `failed` with the reason, the PVC is
   retained, and the finalizer can be re-run.
5. **GC** — PVCs whose sandbox is gone and carry no intent; Jobs whose PVC is
   gone. The same shape as the orphan-pod GC that already exists.

### The finalizer is a job

Not a bespoke controller: a `job.Request` whose artifacts are the sandbox's
post-phase ones and whose volume is the sandbox's workspace. That buys, for
free, everything the punted question needed — retries, a status to poll,
callbacks that actually deliver the outcome, the artifact runner, and S3
credentials that never enter the sandbox.

It also means the failure mode is one an operator already knows how to read: a
failed job, with logs and an event, rather than a line in a sidecar's log that
vanished with its pod.

### Status

| condition | status |
| --- | --- |
| pod exists | `creating` / `ready` / `failed`, as now |
| pod gone, PVC intent pending or running | `finalizing` |
| pod gone, finalizer failed | `failed`, with the job's error, retained |
| PVC gone | `404` |

### What it costs, plainly

- **Creates get slower.** Dynamic provisioning and attach, on top of the cold
  start a poolless sandbox already pays. This is not the shape for creating
  sandboxes at a rate; that is what pools are for, and pools cannot have this.
- **A node-local PV pins the sandbox to a node.** RWX removes that and costs I/O.
- **Quota becomes real.** A PVC per sandbox needs a size cap in validation and a
  `ResourceQuota` in the namespace, or one caller fills a node.
- **Two more things to garbage-collect**, and the sandbox stops being a single
  object. The pod-is-the-record simplicity survives only for ephemeral sandboxes.
- **A node-local PV is already a persistent folder.** If session-to-session
  continuity on one node is all that is wanted, binding the same `claim` on the
  next create is the whole feature and the archive/upload is unnecessary. The S3
  round trip buys durability against losing that node, and portability off it —
  which is a different requirement, worth naming before building either.

### What it does not do

Snapshot a *running* sandbox — that is the explicit endpoint below, and it stays
the answer for ephemeral ones. It does not give pools durable workspaces, and it
does not migrate a workspace between nodes.

### Sequencing

1. **`POST /v1/sandbox/{id}/snapshot`** — synchronous, while the sandbox is alive
   and healthy, outcome in the status code. Small, works for every sandbox
   including ephemeral ones, retryable by the caller, and it makes the
   persistent-folder pattern deterministic rather than best-effort. Restore by
   mount already works, so this closes the loop on its own.
2. **The durable workspace and the finalizer job**, as above, for callers who
   want teardown to snapshot itself without being asked.
3. ~~**Delete the SIGTERM path**~~ — **done.** The post-phase-at-teardown
   mechanism and the `202`/`finalizing` API it needed are gone. With a delta
   pushed on an interval, the flush on the way out costs an interval when it is
   missed rather than the session, so there is nothing for a caller to wait on
   and `DELETE` is `204` again. The `workload` dependency sentinel and the
   `terminationGracePeriodSeconds` dimension stayed: the first is independently
   correct, and the second still makes the final flush more likely to land.

## Mounting the bucket instead

The most interesting objection to all of the above: if the folder is meant to
live in S3, why copy it there at teardown rather than putting it there in the
first place? A FUSE mount over a bucket makes the workspace *be* the durable
store, and the whole problem this document opened with — a dying pod owing work
— stops existing. No trigger, no grace period, no finalizer, no PVC lifecycle,
no node pinning, and restore is a mount rather than a download.

That is a real answer, and for some data it is the right one. It is not a
general replacement for the workspace, for one reason.

### S3 is not a filesystem

The gap is not performance, it is semantics:

| POSIX expects | S3 gives |
| --- | --- |
| atomic `rename()` | copy + delete, O(size), non-atomic |
| in-place partial writes | rewrite the whole object |
| `stat` at memory speed | a network round trip |
| locking, `fsync` ordering | neither |
| directories | a shared key prefix |

An agent workspace is exactly the workload that leans on all five: `git`,
`pip install`, `npm i`, a compiler writing temp files, sqlite. Those are
thousands of small files and renames — the pattern that turns a bucket mount
from "slower" into "pathological", and in the rename case into "silently not
atomic". Meanwhile the shape a bucket mount is *good* at — large objects, read
mostly, written whole — is precisely the shape our existing `mount` artifact
already serves from an erofs or squashfs image, at local-disk speed.

JuiceFS and friends close the semantic gap by keeping metadata in a real
database and chunking data into the bucket. That works, and it is a stateful
service with its own HA story to run and back up. It is a platform decision, not
a sandbox feature.

### Where it would fit here

Two implementations, and the difference matters more than it looks:

**In the sidecar, as another artifact type.** `{"type": "bucket", "in":
"s3://acme/sessions/42/", "out": "data"}`, mounted by the sidecar and reaching
the workload through the propagation we already ship and have
[measured](../sandboxes.md#mounting-a-filesystem-image). The credentials stay on
the sidecar's side of the container boundary, which is the same argument that
put the upload there. Release already tears mounts down. It is a genuinely small
extension: a FUSE binary in the sidecar image, `/dev/fuse`, and a daemon that
outlives the claim — which the sidecar already does.

**As a CSI driver** (`mountpoint-s3`, `juicefs`, `csi-s3`). The mount happens in
the node plugin and is bind-mounted in, so **the sandbox pod needs no privilege
at all** — better than what we do for loop mounts today, not worse. The cost is
a cluster dependency the operator installs and we do not control.

### The catch that decides it

FUSE needs `/dev/fuse`, and sandbox pools are the place we tell people to run
[gVisor](../sandboxes.md#isolation) because untrusted code is the expected
workload. gVisor's FUSE support is partial and not something to assume — the
same tension as loop mounts, but worse, because here it would gate the *whole*
persistence story rather than one artifact type. **This needs the same spike the
mount question got**, and the answer changes which implementation is viable: if
gVisor cannot host the mount, the CSI route (mount outside the sandbox, bind in)
may still work where an in-sidecar daemon cannot.

### What I would actually do with it

Not as the workspace. As a second path, next to it:

- the **workspace** stays local and POSIX — fast, cheap, ephemeral, where builds
  and package managers run;
- a **bucket mount** appears at its own path for the data that wants to be
  durable, so results are in S3 as they are written and nothing has to happen at
  teardown.

That composes with everything already shipped, needs no lifecycle machinery, and
is honest about which half of the workload it serves. It also leaves the
[explicit snapshot](#sequencing) as the answer for "capture this whole workspace,
including the parts a bucket cannot represent".

## Write-back caching, and the reason it changes the argument

The objection to a bucket mount is that every operation is a network round trip
against something that is not a filesystem. A cache in front of it answers most
of that: reads and writes hit local disk, and a background loop pushes to the
bucket. The family exists and is well travelled:

| | metadata | semantics | dependency |
| --- | --- | --- | --- |
| **JuiceFS** | its own database (Redis/Postgres/TiKV) | full POSIX — atomic rename, locking, random writes | a metadata service to run, scale and back up |
| **rclone `--vfs-cache-mode full`** | none | good enough for whole-file work; rename is still remote | none |
| **s3fs `use_cache`** | none | whole-file cache, upload on close | none |
| **Alluxio** | its own | POSIX-ish, built for analytics | a cluster |

JuiceFS is the productised version of exactly what the question describes, and
it is the only one in that list that makes a workspace behave like a disk. The
price is a stateful service whose loss loses the filesystem — the metadata is
not reconstructible from the bucket.

### It does not remove the teardown problem. It makes it survivable.

This is the part worth being precise about, because it reframes everything
above. "Syncs in the background" means that at any instant some writes exist
only in the local cache. If the pod dies — node loss, eviction, a SIGKILL after
the grace period — those writes are gone. So a flush at teardown is still
wanted.

But the flush is a completely different proposition from the archive-and-upload
this document started with:

- it is proportional to **what changed recently**, not to the whole workspace, so
  it finishes in seconds rather than needing a grace period sized to a tarball;
- failing to run costs **the last few seconds of work**, not the entire session.

That second point is what changes the design. The SIGTERM trigger was
unacceptable because missing it lost everything since create — the difference
between a persistent folder and no persistent folder. With continuous
background sync, missing it loses a bounded tail. A best-effort hook is a
reasonable thing to build on top of a durable-by-default store; it is not a
reasonable thing to build a durable store *out of*.

So the sequencing inverts. Rather than making teardown reliable enough to carry
persistence, make persistence continuous and let teardown be the optimisation it
should have been.

### What this would look like here

The cheapest version reuses machinery that already shipped. A writable
`mount` today is an erofs or squashfs image with a **tmpfs** overlay on top; the
overlay's upper directory is precisely the set of changes. Point that upper at
local disk instead of tmpfs and sync it, and you have delta-only persistence
without a FUSE daemon, a metadata service, or a bucket mount at all:

- restore: download the image, mount it read-only, overlay on top — **already
  works**, and it is one download plus an O(1) mount;
- sync: push the upper directory, which is small by construction;
- flush at teardown: the same sync, bounded by what changed since the last one.

The wrinkle noted earlier — that `archive` over a mount point captures the merged
view rather than the delta — is the same observation from the other side. Making
the upper directory addressable is what turns a full snapshot into an
incremental one, and it is a smaller change than any of the alternatives in this
document.

The FUSE catch still applies to JuiceFS and rclone (both need `/dev/fuse`, and
gVisor's support is partial), which is another reason the overlay route is worth
pricing first: it needs no FUSE at all.

## What we would actually build

Concretely, for the overlay-delta route: **four changes across three files.**
Everything else already exists and shipped.

A writable `mount` today is an erofs or squashfs image loop-mounted read-only at
`<out>.lower`, with an overlay stacked at `<out>` whose upper and work layers
live on a **tmpfs** at `<out>.scratch`. The upper directory is therefore already
at a known path, and it already contains exactly the delta — measured:

```
OVERLAY-ON-EMPTYDIR: OK
--- upper (the delta):
  /w/scr/upper/new.txt     created
  /w/scr/upper/base.txt    copied up because modified
/dev/vdb1  btrfs  …  /w    emptyDir, not tmpfs
```

Unmodified files from the image never enter the upper. That is the whole
mechanism; the rest is plumbing.

### 1. Put the upper on disk instead of tmpfs — `internal/sidecar/mount_linux.go`

When a `sync` target is set, skip the tmpfs mount and `MkdirAll` the scratch
directory on the workspace volume. Overlayfs needs upper and work on one
filesystem that supports `trusted.overlay.*` xattrs; the emptyDir qualifies
(verified above on btrfs).

What is lost: the tmpfs `size=` cap. The replacement is the emptyDir's own
`sizeLimit`, which the pod spec can set — a change in the pod builders, not here.

### 2. A `sync` target on the mount artifact — `internal/artifact/mount.go`

```jsonc
{"id": "tree", "type": "mount", "in": "base.erofs", "out": "work",
 "writable": true,
 "sync": "s3://acme/sessions/42.tgz",
 "syncIntervalSeconds": 30}
```

One field and an interval, plus validation. `sync` names where the overlay's
delta is kept; the sandbox restores from it at create, pushes to it on the
interval, and flushes to it on the way out. The rest of the request is unchanged,
and a mount with no `sync` behaves exactly as it does today — tmpfs upper,
nothing uploaded.

### 3. Restore, sync, and flush — `internal/sidecar/runner.go`

All three reuse artifact types that already exist, run through the runner that
already holds the S3 credentials:

- **restore**, during `Mount` and before the overlay is stacked: if the object
  exists, `download` + `unarchive` into the upper directory;
- **sync**, a goroutine started after the mount: `archive` the upper + `upload`,
  every `syncIntervalSeconds`;
- **flush**, in `Release`: the same sync once more.

The sync is roughly sixty lines, because it builds two artifacts in memory and
hands them to the runner. It is not new I/O code.

### 4. Start and stop it — `internal/proxy`

The sidecar already calls `Mount` on the claim and `Release` after draining. The
loop starts in the first and stops in the second. No new lifecycle.

### What we do not build

No FUSE daemon, no `/dev/fuse`, no CSI driver, no metadata service, no PVC
lifecycle, no finalizer job, no new controller, no new endpoint — and no gVisor
question beyond the one mounts already have. The `202`/`finalizing` API this
document proposed becomes unnecessary too: with a sync every thirty seconds,
missing the final flush costs thirty seconds of work, so `DELETE` can stay `204`
and the flush is an optimisation rather than a promise.

### What it costs, and what is still open

- **The sync is crash-consistent, not atomic.** The workload keeps writing while
  the upper is archived. The final flush runs after the drain, when nothing is
  serving, so the last one is clean; intermediate ones are a best-effort restore
  point. Freezing writes mid-session would need cooperation from the workload,
  which we do not have.
- **Cost grows with the delta.** Each cycle archives the whole upper, not the
  change since the last cycle, so a long session re-uploads an increasingly large
  archive. Fine for a first version, and the fix is per-file sync rather than
  archive-and-upload, which is a bigger change than this one.
- **It needs the mount capability**, so the privileged sidecar and everything
  said about it applies unchanged. A sandbox that only wants a persistent folder
  now pays for a privileged container, which is a real objection and the reason
  the bucket-mount-by-CSI route stays interesting.
- **The emptyDir sizeLimit becomes the guard rail** in place of the tmpfs cap,
  and an unbounded upper on a node's disk is a noisy-neighbour problem worth
  capping before this ships.

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
