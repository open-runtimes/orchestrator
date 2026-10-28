# Node artifact cache

**Status: proposal.** It builds on two things that exist: the [node mount agent](node-mount-agent.md) and per-transfer S3 grants (`internal/artifact/grant.go`, [jobs: S3 URLs](../jobs.md#s3-urls)). Nothing here is built yet.

## The problem

The common shape of a workload's artifacts is a download that feeds a mount. Edge's function and site runtimes are exactly that (`src/Edge/K8s/Runner.php`):

```json
[
  {"id": "source", "type": "download", "in": "s3://builds/<deployment>.erofs", "out": "source.erofs"},
  {"id": "code", "type": "mount", "in": "source.erofs", "out": "code", "writable": true, "depends": "source"}
]
```

Every pod does that download itself, into its own workspace. So:

- **The same bytes cross the network once per pod, not once per node.** A deployment that scales to ten replicas on three nodes downloads its build ten times. A cold start always pays the full download, even when another replica on the same node finished the same download a second ago.
- **The same bytes sit on disk once per pod.** Every workspace keeps its own copy of the image for as long as the pod lives.
- **The download is on the cold-start path** and it is the part of it that grows with the build. Mounting is milliseconds (`LOOP_CONFIGURE` + `mount(2)`); the download is not.

## The proposal

When the mount agent is on, it also fetches. A download that feeds a mount is no longer done by the sidecar into the workspace. The sidecar hands the agent the URL and the pod's grant for it. The agent fetches the object once per node into a node-local cache and loop-mounts the cached file read-only at `out`. With `writable`, the overlay and its upper are the pod's, as today.

```
claim / pre phase ─► sidecar: fetch-and-mount <url> at <out>, here is my grant
                  ─► agent: redeem the grant at the plane's service ─► pre-signed GET URL
                  ─► agent: probe the object (ranged GET, 1 byte) ─► ETag
                  ─► hit:  cache has (url, ETag) ─► loop-mount the cached file
                  ─► miss: download once (concurrent misses wait on one download) ─► then mount
```

The artifact API does not change. A `download` followed by a `mount` of its output is what triggers the agent path; the runner recognises the pair. A download nothing mounts (an archive to unpack, a file the workload reads) keeps going through the sidecar, as do uploads and mount `sync`, which are per-pod writes and never cacheable.

Without the agent, nothing changes: the sidecar downloads with its grants, as it does now. The cache is a speed-up, never a requirement, and turning the agent off falls back cleanly.

### Authorization: the grant, checked on every hit

The cache is shared by every pod on the node, across tenants, so a hit must never be cheaper to get than a miss. **The agent redeems the pod's grant on every request, hit or miss.** The service only pre-signs a URL when the grant is valid for exactly that object and method, so a pod that has no grant for an object cannot learn whether it is cached, let alone read it. The agent holds no S3 key and no grant key; it only relays the pod's grant.

Redeeming on a hit costs one call to the plane's service. That is the price of not trusting the URL a pod names, and it is small next to a download.

### Freshness: URL plus ETag

The cache key is the object URL and its ETag. Before using a cached file, the agent probes the object with the pre-signed URL: a ranged `GET` of one byte, which returns the current `ETag` header. (A `HEAD` would fail: the URL is signed for `GET`.) A changed object is a miss and is downloaded again; the old entry ages out.

Most artifacts never change. Edge writes each build once, under a key unique to the deployment. But the probe means correctness does not depend on that, and it costs one round trip to the object store.

Keying by content digest instead (an optional `sha256` on `download`, as the mount agent doc suggests) would dedupe identical bytes under different URLs, but it opens a hole: a pod naming its own URL with another tenant's digest would hit the other tenant's entry. Closing that needs the URL-to-digest binding checked anyway, which is what URL plus ETag already is. A `sha256` is still worth adding separately, verified before mounting, because the host kernel parses the image (see the mount agent doc). It is an integrity check, not a cache key.

### The agent only talks to services it was configured with

The agent runs on the host network, so whatever it fetches, it fetches from the node: the metadata endpoint, the kubelet, anything bound on the host. It must never fetch a URL a pod named. Two rules keep it that way:

- **The presign endpoint comes from the agent's own configuration** (one per plane, set by the chart), never from the request. A pod cannot point the agent at a server that answers with a URL of its choosing.
- **The agent only fetches what that service returns**, a pre-signed URL on the configured S3 endpoint. It refuses anything else, and it follows no redirects.

This replaces nothing in the existing per-pod token check: the token still selects the workspace the mount lands in, exactly as for `/v1/mount`.

### Storage, eviction and concurrency

- **Where:** a directory under the agent's root on the node's disk (`/var/lib/<agent>/cache`), on the same filesystem as the workspaces.
- **Budget:** `nodeAgent.cache.size`, least recently used out first. Eviction unlinks the file. A file still loop-mounted by a running pod keeps its inode until the last mount goes, so eviction never breaks a pod; the disk comes back when that pod ends.
- **Read-only, never hard-linked into a workspace.** The workspace is attacker-writable. A hard link would let one pod rewrite a file other pods mount. The loop device is opened read-only on the cache file, and the writable layer is the pod's own tmpfs upper.
- **One download per key.** Concurrent misses for the same key wait on a single in-flight download (singleflight). A burst of replicas on one node downloads once.
- **Partial files never enter the cache.** Download to a temporary name, `fsync`, rename into place, as `artifact.Download` already does.
- **Restart:** the cache is rebuilt from the directory on start (names encode the key), so an agent restart keeps it.

### What moves where in the code

| Today | With the cache |
| --- | --- |
| `sidecar.Runner` runs `download`, then `mount` through `mountagent.Client` | the runner sends a `download`→`mount` pair to the agent as one `/v1/fetch-mount`; everything else is unchanged |
| the agent mounts a workspace file | the agent can also mount a cache file it owns |
| the sidecar redeems grants for every download | the agent redeems them for fetch-mounts, at its configured endpoint |
| `cmd/mount-agent`, `mountAgent.*` | renamed `cmd/node-agent`, `nodeAgent.*`, since it is no longer only a mounter. The CSI driver name stays: changing it orphans the volumes of running pods |

### What it does not change

- **Uploads, `sync` and unmounted downloads** stay in the sidecar, with grants.
- **The Docker backend** has no agent and keeps downloading per container.
- **Isolation.** The cached image is mounted the same way an in-workspace image is today; `hostUsers: false` and gVisor work as the mount agent doc describes.

## Measuring it

The win is cold start on edge, and edge measures it already: the sidecar's per-artifact reports restore the `runtime_sidecar_*` phase timings, and `edge_runtime_duration_seconds` covers create to serving. Compare the download phase and the total by `node_pool`, cache on against off, over a day of real traffic. Also export from the agent:

- hits, misses and probe failures, by plane
- bytes served from cache against bytes downloaded
- download and probe latency
- cache size and evictions

A hit should cost two round trips (redeem, probe) instead of a download. If the hit rate on edge is low, because replicas of one deployment rarely share a node, the cache buys little and the right move is scheduling, not caching.

## What it costs

- **The agent becomes a network client** of the plane services and the object store, from the host network. The two rules above are what keep that safe, and they need tests that try to break them.
- **Disk on every node**, bounded by the budget, outside kubelet's disk-pressure accounting. The agent should report its usage the same way the loop-backed workspace proposal does.
- **The agent is on the cold-start path for every mounting pod.** It already is for the mount; now it is for the download too. An agent that is down fails those pods' starts, as it does today for mounts.

## What I would do next

1. **Build it in orchestrator for jobs and revisions**, behind `nodeAgent.cache.enabled`, with kind tests that cover: a hit and a miss; a changed ETag; a pod with no grant for a cached object (refused, and it cannot tell the object is cached); a request naming its own presign endpoint (refused); concurrent misses downloading once; eviction under a running mount.
2. **Rename the agent** in the same release, keeping the CSI driver name.
3. **Edge adopts grants** (it still puts storage keys in runtime pods), **then the agent** (it still mounts from a privileged sidecar), **then the cache**. Each step is useful on its own, in that order.
4. **Measure on one edge region** before turning it on everywhere, and keep it per pool until the hit rate says otherwise.
