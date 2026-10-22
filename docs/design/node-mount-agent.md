# Node mount agent

**Status: proposal, spiked.** Nothing here is implemented. The propagation questions [the mounts design](sandbox-mounts-and-shutdown-artifacts.md#what-i-would-do-next) left open have been answered on kind (results below), and the answers changed the design more than once. What is left open is listed at the end.

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

The obvious choice is a Unix socket with `SO_PEERCRED`, and it does not survive gVisor. By default runsc refuses connections from the sandbox to host Unix sockets (`--host-uds`), and the peer credentials it would present belong to the sandbox's processes, not the sidecar. The spike did not exercise this; it is what runsc documents. So it is a network call:

- The agent listens on the node (host port or host network). The sidecar reaches it at the downward-API `status.hostIP`.
- The sidecar authenticates with `hex(HMAC-SHA256(agentKey, namespace/podName))`, injected as env at pod creation. That is the scheme warm pods already use for claim tokens (`internal/warm/token.go`), with its own key. The worker container never sees it.
- The agent re-derives the token and then checks the rest itself: the pod is on this node, it has an agent workspace volume, and the request names only paths inside that volume. A request that passes authentication still cannot reach anything but its own workspace.
- A NetworkPolicy keeps the port reachable only from pods that should talk to it. It cannot tell the sidecar from the worker, since they share an IP, which is why the token, not the network, is the boundary.

### The workspace is attacker-writable, so paths are resolved by fd

The worker can write anything in the workspace, including replacing `out` with a symlink to `/etc` between the sidecar's request and the agent's mount. The agent must never resolve `in` or `out` by string:

- Open both with `openat2(RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS)` relative to the per-volume directory fd.
- Configure the loop device from the `in` fd (`LOOP_CONFIGURE`, as `setupLoop` does today).
- Mount onto the `out` fd with `fsopen`/`fsmount`/`move_mount(MOVE_MOUNT_T_EMPTY_PATH)`, so the target is the directory that was checked, not whatever the path names a microsecond later.
- Mount read-only images with `MS_RDONLY | MS_NODEV | MS_NOSUID`, as `mountImage` does now.

### What moves where in the code

| Today | With the agent |
| --- | --- |
| `internal/sidecar/mount_linux.go`: loop, image, overlay, unmount | moves to `internal/mount`, shared by the agent and the in-pod fallback |
| `kube.MountingSecurityContext` on the sidecar | gone in agent mode; the sidecar is hardened like every other container |
| workspace `emptyDir`, `Bidirectional` on the sidecar | workspace CSI volume, `HostToContainer` on the worker and the sidecar |
| sidecar calls the mounter directly | sidecar calls the agent (`Mount`, `Release`) with its token |
| S3 credentials in the sidecar env | unchanged in this change, and the obvious follow-up: once there is a node component, it can own the credentials, and `s3://` downloads can move to it as well |
| `HasMount` means "privileged pod" | `HasMount` means "agent workspace"; pools still key on `mounts: true`, because the volume source differs |

New pieces:

- `cmd/mount-agent`: the CSI Identity and Node services, inline volumes only and no controller, plus the `Mount`/`Release` HTTP API. Built with ko like the other binaries.
- Chart: `mountAgent.enabled`, a DaemonSet (privileged, `/var/lib/kubelet` with `Bidirectional`, `/dev`, `node-driver-registrar`), the `CSIDriver` object, the agent key Secret, and the NetworkPolicy.
- `KUBE_MOUNT_MODE=sidecar|agent`. `sidecar` is today's behaviour and stays the default until the agent has run in production. The Docker backend is unaffected: Docker jobs keep their privileged sidecar, and Docker deployments and sandboxes keep refusing mounts.

## Spike results

Kind (Kubernetes 1.37, containerd 2.3.4, kernel 7.2 arm64), gVisor `release-20261012.0` with `runsc` as a containerd runtime handler. A minimal CSI node plugin published a volume as a shared bind mount. Pods used the same hardening as a hardened workload: drop `ALL`, `RuntimeDefault` seccomp, no privilege escalation. **Every mount was made on the host after the container was already running.**

| Late host mount | runc | gVisor | runc + `hostUsers: false` |
| --- | --- | --- | --- |
| erofs on a subdirectory the container never looked at | ✅ read, exec, 200-file listing | ✅ | ✅ |
| erofs on a subdirectory the container had already listed | ✅ | ✅ | ✅ |
| writable overlay (erofs lower, tmpfs upper) on a subdirectory | ✅ | ✅ | ✅ |
| a mount stacked on the volume root | ✅ | ❌ still shows the old contents | ✅ |
| control: no `HostToContainer` | ❌ not visible, as expected | | |
| plain `emptyDir` workspace instead of CSI | ✅ | ✅ | ✅ |

- **gVisor with `hostUsers: false`** never starts: `gofer: error setting namespace of type user ... invalid argument`. A pool picks one.
- **User namespaces need no ID-mapped mounts.** Files on a read-only image appear as `65534` and are readable and executable. Writes to the overlay land as the pod's mapped host uid and read back as uid 0 inside, because overlayfs performs upper-layer operations with the mounter's credentials.
- **Teardown through `NodeUnpublishVolume`** unmounted all 30 late submounts in the spike and freed every loop device. With a plain `emptyDir`, deletion hung until the mount was removed by hand (the 21s above).
- The plain-`emptyDir` result depends on the node's root mount being `shared`, which is the systemd default. The CSI volume does not depend on it, because the agent marks its own target shared. That is a second, smaller reason to prefer CSI.

## What it costs

- **A cluster component that needs privilege.** It is privileged in one place, ours, instead of in every pod that mounts. That is the trade, and it is a good one, but it is not free: the agent is now the most sensitive thing on the node, and its API is the boundary.
- **Workspace size limits move to us.** kubelet enforces an `emptyDir` `sizeLimit`; it does not enforce anything on a CSI volume. Options: XFS project quotas on the agent's root, a sparse loop-backed filesystem per workspace (one more loop device per pod), or monitoring only. This is the one decision I would want made before building.
- **Download timing is unchanged, deliberately.** The download stays in the pod's pre phase, so this proposal costs no cold-start latency: the mount itself is the same `LOOP_CONFIGURE` + `mount(2)`, just issued from a different process. A node-level cache keyed by content digest (below) is where latency could improve, not regress.

## What it does not change

- **The kernel still parses the image.** A loop mount means the host kernel reads the filesystem's metadata. If untrusted code can influence the image, that is kernel attack surface whether the mount happens in the pod or on the host. An optional `sha256` on `download`, checked by the agent before mounting, closes it for images the platform built. The same digest would let pods on one node share a loop device and its page cache for identical images.
- **Isolation is still opt-in.** The agent makes `hostUsers: false` and gVisor possible for mounting pods; it does not turn them on.

## What I would do next

1. **Settle workspace size limits.** It is the one open design decision and it shapes the agent's storage layout.
2. **Repeat the spike on a production-shaped node** (amd64, the operator's kernel and containerd) before writing the agent, because the gVisor results in particular are version-sensitive.
3. **Build it behind `KUBE_MOUNT_MODE=agent`**, jobs first (no warm pods, simplest lifecycle), then revisions and pools. Add a `k8s_integration` test on kind that covers propagation into a running container and teardown, and a unit suite for the `openat2` path resolution with symlink races.
4. **Then turn on `hostUsers: false` for mounting pools.** It has no runtime cost, and it is most of what gVisor would buy.
