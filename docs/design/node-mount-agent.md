# Node mount agent

**Status: implemented for jobs, behind `KUBE_MOUNT_MODE=agent`** (`cmd/mount-agent`, `internal/mountagent`, `internal/mount`; chart `mountAgent.*`, `jobs.mountMode`). Deployments, pools and sandboxes still mount from the privileged sidecar, and the loop-backed workspace and user-namespace ID mapping below are not built yet. The propagation questions [the mounts design](sandbox-mounts-and-shutdown-artifacts.md#what-i-would-do-next) left open have been answered on kind and then on a production-shaped DOKS node, gVisor included (results below), and the answers changed the design more than once. The workspace size limit and the cold-start cost of each isolation tier are measured too.

## The problem

A `mount` artifact is performed inside the workload's own pod by a container running `Privileged` with `RunAsUser: 0` (`kube.MountingSecurityContext`, `internal/kube/pod.go:53`). That is the job sidecar's post phase for jobs and the workload sidecar (`internal/proxy`) for revisions and sandboxes. It gets `Bidirectional` propagation on the shared volume, and the worker gets `HostToContainer`.

That has three costs, and they compound:

1. **Every mounting pod carries a privileged container** next to untrusted code, sharing its network and IPC namespaces. It is our image, not theirs, but it is root with every capability in a pod whose whole purpose is running someone else's code.
2. **It rules out the stronger isolation tiers.** gVisor cannot host a privileged container or hand out loop devices. Kubernetes user namespaces (`hostUsers: false`) cannot be enabled on a pod that contains a privileged container. Kata has the same constraint. Today the mounts design scopes mounts to `runc` pools for exactly this reason.
3. **The S3 credentials ride along.** `s3://` downloads are signed with the service's static credentials, which are copied into the sidecar's environment in every pod (`warm/pod.go:164`, the deployment and job mappers). That one isn't caused by the privilege, but the fix for the privilege is a natural place to fix it too.

Extraction (`unarchive`) avoids all three and is the wrong answer: mounting is what makes a large read-mostly tree start fast, and it stays.

## The proposal

Do the mount on the node, not in the pod. A small DaemonSet, the **mount agent**, owns loop devices and `mount(2)`. Pods ask it to mount; they never need privilege to do it.

```
pod created ─► kubelet ─► agent NodePublishVolume: create the workspace, bind it in
claim / pre phase ─► sidecar downloads into the workspace (unchanged, unprivileged)
                 ─► sidecar asks the agent: mount <in> at <out>, in my workspace
                 ─► agent: loop + mount (+ tmpfs overlay) on the host, under the workspace
                 ─► HostToContainer propagation: the mount appears in the running worker
                 ─► sidecar signals the shim / marks mounts ready (unchanged)
pod deleted  ─► kubelet ─► agent NodeUnpublishVolume: unmount everything, deepest first
```

### The artifact API does not change

`in` is a workspace file an earlier artifact produced (usually a `download`), and `out` is a workspace directory (`internal/artifact/mount.go`). The workspace exists on the host, so the agent can read `in` and mount at `out` without the pod doing anything privileged. `writable`, `size`, `sync` and `syncIntervalSeconds` keep their meaning:

- **`writable`/`size`:** the agent creates the tmpfs upper on the host. Pages are charged to the cgroup of the process that writes them, which is the worker, so the pod's memory limit still bounds it.
- **`sync`:** unchanged and still owned by the sidecar. With `sync` set the upper already lives in the workspace, so the sidecar keeps restoring, pushing and flushing as it does now (`internal/sidecar/runner.go`). The agent unmounts only at volume teardown, after the containers have exited, so the final flush still happens first.

Two new validation rules, both from the spike:

- `out` must be a subdirectory of the workspace, never the workspace root. gVisor does not see a mount stacked on the root of a volume after the sandbox has opened it.
- `runtimeClass: gvisor` cannot be combined with `hostUsers: false`. The sandbox fails to start (see results).

### The workspace becomes a CSI volume

The obvious version is to keep the workspace an `emptyDir` and have the agent mount into it. Propagation works for that (see results), but teardown does not. While a host mount is inside an `emptyDir`, kubelet cannot remove it: the pod hangs in `Terminating` and kubelet retries on its backoff. In the spike, the pod finished deleting 21s after the mount was removed. A stuck pod also blocks deletion of its namespace.

So the workspace is an **inline ephemeral CSI volume** served by the agent (`attachRequired: false`, `podInfoOnMount: true`, `volumeLifecycleModes: [Ephemeral]`):

- **NodePublishVolume** creates a per-volume directory under the agent's root, bind-mounts it on the target, and records `volumeID → pod`. Anything else the agent mounts for that pod (lowers, tmpfs scratch) lives under the same per-volume directory, never next to it.
- **NodeUnpublishVolume** unmounts everything under both, deepest first, and removes the directory. Loop devices are configured with autoclear, so they go with their last mount. This runs before kubelet considers the volume gone, so nothing is ever busy when kubelet looks.

This also gives the agent the only lifecycle hook it needs. There is no pod informer and no reconciler.

### How the sidecar reaches the agent

The obvious choice is a Unix socket with `SO_PEERCRED`, and it does not survive gVisor. runsc refuses connections from the sandbox to host Unix sockets unless it runs with `--host-uds=open` (the flag's default is `none` in `release-20261012.0`). Even with it, the peer credentials would belong to the sandbox's processes, not the sidecar. So it is a network call:

- The agent listens on the node (host port or host network). The sidecar reaches it at the downward-API `status.hostIP`.
- The sidecar authenticates with a token the mapper mints per pod: 32 random bytes, placed in the workspace volume's attributes (where only kubelet and the agent read it) and in the sidecar's environment. The worker container sees neither. I first planned an HMAC of the pod name, like warm-pod claim tokens, but a Job's pod name does not exist when the spec is built, and a random token needs no shared key at all.
- The agent learns the token when kubelet publishes the volume and stores only its hash. A presented token selects exactly one workspace, and every path in the request is then resolved beneath it. A request that passes authentication still cannot reach anything but its own workspace, and the token stops working when the volume is unpublished.
- A NetworkPolicy keeps the port reachable only from pods that should talk to it. It cannot tell the sidecar from the worker, since they share an IP, which is why the token, not the network, is the boundary.

### The workspace is attacker-writable, so paths are resolved by fd

The worker can write anything in the workspace, including replacing `out` with a symlink to `/etc` between the sidecar's request and the agent's mount. The agent must never resolve `in` or `out` by string:

- Open both with `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS)` relative to the per-volume directory fd.
- Configure the loop device from the `in` fd (`LOOP_CONFIGURE`, as `setupLoop` does today).
- Mount onto the `out` directory as `/proc/self/fd/N`, so the target is the directory that was checked, not whatever the path names a microsecond later. Read-only binds of an extracted directory are cloned detached, made read-only while detached, and attached by descriptor (`open_tree` → `mount_setattr` → `move_mount`), so they are never visible writable.
- Reopen a path after mounting on it: a descriptor taken before a mount still points underneath it. That is the bug the first run of the tests found.
- Unmount through the parent's descriptor (`/proc/self/fd/<parent>/<name>`, `UMOUNT_NOFOLLOW`). A descriptor on the mount itself is a reference to it, and the kernel calls the mount busy for as long as the caller holds it; teardown first took the full 10-second busy deadline for exactly that reason.
- Mount images, tmpfs and overlays `nosuid` as well as `nodev`.

### What moves where in the code

| Today | With the agent |
| --- | --- |
| `internal/sidecar/mount_linux.go`: loop, image, overlay, unmount | moves to `internal/mount`, shared by the agent and the in-pod fallback |
| `kube.MountingSecurityContext` on the sidecar | gone in agent mode; the sidecar keeps file access (root with only `CAP_DAC_OVERRIDE`) for sync and post-job artifacts, and nothing more |
| workspace `emptyDir`, `Bidirectional` on the sidecar | workspace CSI volume, `HostToContainer` on the worker and the sidecar |
| sidecar calls the mounter directly | the runner is unchanged; its `Mounter` is an agent client (`mountagent.Client`), so restore, sync, adoption and the ready marker all work as before |
| S3 credentials in the sidecar env | unchanged in this change, and the obvious follow-up: once there is a node component, it can own the credentials, and `s3://` downloads can move to it as well |
| `HasMount` means "privileged pod" | `HasMount` means "agent workspace"; pools still key on `mounts: true`, because the volume source differs |

New pieces:

- `cmd/mount-agent`: the CSI Identity and Node services, inline volumes only and no controller, plus the mount API (`/v1/mount`, `/v1/unmount`, `/v1/mounted`). Built with ko like the other binaries.
- Chart: `mountAgent.enabled`, a DaemonSet (privileged, host network, `/var/lib/kubelet` with `Bidirectional`, `/dev`, `node-driver-registrar`) and the `CSIDriver` object. No Secret, since tokens are per pod. No NetworkPolicy either: the agent runs on the host network, where pod policies do not apply, and the token is the boundary anyway.
- `KUBE_MOUNT_MODE=sidecar|agent`. `sidecar` is today's behaviour and stays the default until the agent has run in production. The Docker backend is unaffected: Docker jobs keep their privileged sidecar, and Docker deployments and sandboxes keep refusing mounts.

## Spike results

Kind (Kubernetes 1.37, containerd 2.3.4, kernel 7.2 arm64), gVisor `release-20261012.0` with `runsc` as a containerd runtime handler. A minimal CSI node plugin published a volume as a shared bind mount. Pods used the same hardening as a hardened workload: drop `ALL`, `RuntimeDefault` seccomp, no privilege escalation. **Every mount was made on the host after the container was already running.**

| Late host mount | runc | gVisor | runc + `hostUsers: false` |
| --- | --- | --- | --- |
| erofs on a subdirectory the container never looked at | ✅ read, exec, 200-file listing | ✅ | ✅ |
| erofs on a subdirectory the container had already listed | ✅ | ✅ | ✅ |
| writable overlay (erofs lower, tmpfs upper) on a subdirectory | ✅ | ✅ | ✅ on kernel 7.2; on 6.12 it needs ID mapping (below) |
| a mount stacked on the volume root | ✅ | ❌ still shows the old contents | ✅ |
| control: no `HostToContainer` | ❌ not visible, as expected | | |
| plain `emptyDir` workspace instead of CSI | ✅ | ✅ | ✅ |

- **gVisor with `hostUsers: false`** never starts: `gofer: error setting namespace of type user ... invalid argument`. A pool picks one.
- **Teardown through `NodeUnpublishVolume`** unmounted all 30 late submounts in the spike and freed every loop device. With a plain `emptyDir`, deletion hung until the mount was removed by hand (the 21s above).
- The plain-`emptyDir` result depends on the node's root mount being `shared`, which is the systemd default. The CSI volume does not depend on it, because the agent marks its own target shared. That is a second, smaller reason to prefer CSI.

### On a production-shaped node

DOKS, amd64, kernel 6.12.96, containerd 2.2.3, kubelet 1.36.3, plain `emptyDir` workspaces, mounts made from the host mount namespace by a privileged helper. gVisor (`release-20261012.0`) was installed on a throwaway, tainted node with one containerd drop-in (`/etc/containerd/conf.d/`, which DOKS's containerd already imports) and a containerd restart. No edit to the main config was needed.

- **Propagation is the same as on kind, for all three tiers.** Late erofs mounts on fresh and already-listed subdirectories appear in running runc, gVisor and `hostUsers: false` containers. A plain writable overlay works under runc and gVisor. gVisor again does not see a mount stacked on the volume root. The control pod without `HostToContainer` sees nothing. `hostUsers: false` pods start with no node changes.
- **gVisor with `hostUsers: false` is refused by kubelet** before runsc is involved: `RuntimeClass handler "runsc" does not support user namespaces`.
- **gVisor holds late mounts busy while the sandbox runs.** The host cannot unmount them (`target is busy`) until the sandbox exits; afterwards they unmount cleanly. Teardown is unaffected, because kubelet only tears volumes down after the containers have stopped. But an agent `Release` while a gVisor pod is still running has to detach lazily (`MNT_DETACH`) and let the sandbox drop its references.
- **Writable overlays in user-namespaced pods differ from kind, and they are the one case that needs care.** On kernel 6.12 the container gets `Permission denied` writing to an overlay whose layers belong to host root. (On kind's kernel 7.2 the same overlay was writable.) What I tried:

  | Overlay setup | Result in the `hostUsers: false` pod |
  | --- | --- |
  | plain layers, mounted by host root | read-only files show as `65534`; writes refused |
  | ID-mapped bind of the finished overlay | `mount_setattr: Invalid argument`: overlayfs cannot itself be ID-mapped |
  | ID-mapped lower **and** ID-mapped tmpfs for upper/work | mounts, but read-only: overlayfs creates its `work/` directory as the mounter, which has no ID in that mapping (`EOVERFLOW`), so it falls back to read-only |
  | **ID-mapped lower, plain tmpfs for upper/work owned by the pod's root host uid** | ✅ files show as uid 0; writes at the root, modifying an image file, `mkdir` inside an image directory, and deleting an image file all work. The upper holds the pod's mapped uid on the host |

  So in user-namespace mode, the agent ID-maps the erofs lower layer into the pod's user namespace (`mount_setattr(MOUNT_ATTR_IDMAP)` with `/proc/<pid>/ns/user` of any process in the pod). It also creates the upper and work directories owned by the host uid that the pod's root maps to (read from the same process's `uid_map`). Read-only mounts should be ID-mapped too, so code shows as root-owned rather than `65534`. That is cosmetic for reading, but it matches what runtimes expect.

### Workspace size limits

DOKS nodes run ext4 on a single disk with `Quota mode: none`. Project quotas cannot be enabled on a mounted root filesystem, so filesystem quotas are not an option there, and they would not be on most managed node images. That also means an `emptyDir` `sizeLimit` is not a hard limit on these nodes today: without quotas, kubelet measures usage periodically and evicts the pod.

A sparse, loop-backed ext4 per workspace (no journal, lazy inode-table init), measured on the dev machine:

| | ext4, no journal, 2 GiB | ext4, no journal, 10 GiB | ext4 with journal, 2 GiB | xfs, 2 GiB |
| --- | --- | --- | --- | --- |
| `mkfs` | 11ms | 15ms | 12ms | 5ms |
| `mount` | 3ms | 4ms | 6ms | 5ms |
| on-disk overhead | 1.2 MB | 4.3 MB | 1.7 MB | 65 MB |

- **Hard limit:** writes stop with `ENOSPC` at the size, and usage is a `statfs` instead of a tree walk.
- **Steady-state cost:** none measurable. Creating 3,000 small files in 30 directories took 29–37ms through the loop device against 33–35ms on the host filesystem, and reading them back was the same. An explicit `sync` is slower: about 20–40ms against 15–18ms.
- **The first seconds after mount are slower,** because ext4 zeroes inode tables in the background. A pool of pre-formatted images the agent keeps ready would take both that and the 15ms off the startup path.
- **What it costs:** one loop device per pod. Space is not attributed to the pod in kubelet's disk-pressure eviction ranking (the sparse file lives on the node's root filesystem), so the agent should report per-workspace usage itself.

So: **a loop-backed ext4 workspace, no journal**, sized from the same field `sizeLimit` comes from today.

### Cold-start cost of each isolation tier

Kind (arm64), a Node 22 app with `express` and an SDK (939 files, 8.4 MB) on an erofs mount, timed from pod creation to the first HTTP 200, 10 runs each after warm-up:

| | create → HTTP 200, p50 / p90 | module loading (p50) | Node start → listening (p50) | warm request, p50 / p99 |
| --- | --- | --- | --- | --- |
| runc | 564 / 572ms | 46ms | 55ms | 0.09 / 0.62ms |
| runc + `hostUsers: false` | 575 / 595ms | 46ms | 56ms | 0.09 / 0.47ms |
| gVisor | 771 / 1072ms | 178ms | 208ms | 0.28 / 2.81ms |

The same measurement on the DOKS node (amd64), with the driver running in a pod on that node so every timestamp comes from one clock:

| | create → HTTP 200, p50 / p90 | module loading (p50) | Node start → listening (p50) |
| --- | --- | --- | --- |
| runc | 1196 / 1335ms | 170ms | 210ms |
| gVisor | 1544 / 1713ms | 387ms | 468ms |
| runc, second batch (20 runs) | 1267 / 1373ms | | |
| runc + `hostUsers: false` (20 runs, same batch) | 1266 / 1321ms | | |

- **User namespaces are free on both.** The first DOKS batch suggested +150ms. A second batch of 20 runs each, alternating order, put `hostUsers: false` and runc within a millisecond at p50.
- **gVisor costs about 350ms at p50 on DOKS** (about 200ms on the laptop). Part of it is a slower sandbox start. The larger part is module loading, 2.3× slower on DOKS and 3.9× on the laptop, because every file open and stat goes through gVisor's file proxy. That share grows with the number of files an app loads at startup.
- **Warm requests are cheap under every tier.** gVisor's network stack adds a fraction of a millisecond (measured on the laptop).

## What it costs

- **A cluster component that needs privilege.** It is privileged in one place, ours, instead of in every pod that mounts. That is the trade, and it is a good one, but it is not free: the agent is now the most sensitive thing on the node, and its API is the boundary.
- **Workspace size limits move to us.** kubelet enforces nothing on a CSI volume. The loop-backed workspace above is a harder limit than today's on nodes without quotas, at about 15ms per pod and one loop device.
- **User namespaces need the agent to know the pod's user namespace.** Writable overlays need an ID-mapped lower layer and an upper owned by the pod's mapped root, so the agent has to find a process in the pod and read its user namespace. Pods without `hostUsers: false` skip all of it.
- **Download timing is unchanged, deliberately.** The download stays in the pod's pre phase, so this proposal costs no cold-start latency: the mount itself is the same `LOOP_CONFIGURE` + `mount(2)`, just issued from a different process. A node-level cache keyed by content digest (below) is where latency could improve, not regress.

## What it does not change

- **The kernel still parses the image.** A loop mount means the host kernel reads the filesystem's metadata. If untrusted code can influence the image, that is kernel attack surface whether the mount happens in the pod or on the host. An optional `sha256` on `download`, checked by the agent before mounting, closes it for images the platform built. The same digest would let pods on one node share a loop device and its page cache for identical images.
- **Isolation is still opt-in.** The agent makes `hostUsers: false` and gVisor possible for mounting pods; it does not turn them on.

## What I would do next

1. ~~**Build it behind `KUBE_MOUNT_MODE=agent`**, jobs first.~~ Done for jobs, with three kinds of test:
   - a `k8s_integration` test on kind that runs read-only and writable mounts through the agent and asserts no container was privileged
   - privileged kernel tests that drive kubelet's publish, the sidecar's client and kubelet's unpublish end to end
   - unprivileged tests for symlink refusal in every `Root` operation
2. **Revisions and pools next.** The warm-pod claim path already mounts after the pod exists, so it needs no new agent behaviour, only the mapper and pool-shape changes.
3. **Loop-backed workspaces**, for a hard size limit (measured above).
4. **User-namespace ID mapping in the agent**, and a writable-overlay test in a `hostUsers: false` pod, because that case behaves differently across kernels. **Then turn on `hostUsers: false` for mounting pools.** It costs nothing at cold start or per request, and it is most of what gVisor would buy.
5. **gVisor stays per pool.** It costs about 350ms at p50 on DOKS, more for apps that load many files. It needs a runsc install per node: one containerd drop-in plus a restart, which a DaemonSet can do, gated by a node label the RuntimeClass selects. An agent `Release` on a running gVisor pod must detach lazily.
