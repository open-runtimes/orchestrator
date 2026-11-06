# Node artifact cache

**Status: proposal.** It builds on the [node mount agent](node-mount-agent.md) and on either of the two ways a pod already proves it may read an object without holding a key: a per-transfer S3 grant (`internal/artifact/grant.go`, [jobs: S3 URLs](../jobs.md#s3-urls)), or a URL pre-signed by whoever created the pod, which is what edge does. Nothing here is built yet.

## The problem

The common shape of a workload's artifacts is a download that feeds a mount. Edge's function and site runtimes are exactly that (`src/Edge/K8s/Runner.php`). With `_EDGE_RUNTIME_PRESIGNED_SOURCE`, which is on in every production region, the download names a pre-signed URL and the pod carries no `S3_*` keys:

```json
[
  {"id": "source", "type": "download", "in": "https://<endpoint>/<bucket>/<deployment>.erofs?X-Amz-Signature=…", "out": "source.erofs"},
  {"id": "code", "type": "mount", "in": "source.erofs", "out": "code", "writable": true, "depends": "source"}
]
```

Every pod does that download itself, into its own workspace. So:

- **The same bytes cross the network once per pod, not once per node.** A deployment that scales to ten replicas on three nodes downloads its build ten times. A cold start always pays the full download, even when another replica on the same node finished the same download a second ago.
- **The same bytes sit on disk once per pod.** Every workspace keeps its own copy of the image for as long as the pod lives.
- **The download is on the cold-start path** and it is the part of it that grows with the build. Mounting is milliseconds (`LOOP_CONFIGURE` + `mount(2)`); the download is not.

## The proposal

When the mount agent is on, it also fetches. A download that feeds a mount is no longer done by the sidecar into the workspace. The sidecar hands the agent the URL and its proof of access: a grant for an `s3://` URL, or nothing extra for a pre-signed URL, which is its own proof. The agent fetches the object once per node into a node-local cache and loop-mounts the cached file read-only at `out`. With `writable`, the overlay and its upper are the pod's, as today.

```
claim / pre phase ─► sidecar: fetch-and-mount <url> at <out> (+ grant, for an s3:// URL)
                  ─► agent: s3:// + grant ─► redeem at the plane's service ─► pre-signed GET URL
                            pre-signed URL ─► check it is on a configured S3 endpoint
                  ─► agent: probe the object with that URL (ranged GET, 1 byte) ─► ETag
                  ─► hit:  cache has (object, ETag) ─► loop-mount the cached file
                  ─► miss: download once (concurrent misses wait on one download) ─► then mount
```

Both paths end in the same place: a pre-signed URL the object store accepted for this pod, minutes ago or now. Everything after the probe is shared.

The artifact API does not change. A `download` followed by a `mount` of its output is what triggers the agent path; the runner recognises the pair. A download nothing mounts (an archive to unpack, a file the workload reads) keeps going through the sidecar, as do uploads and mount `sync`, which are per-pod writes and never cacheable.

Without the agent, nothing changes: the sidecar downloads with its grants or its pre-signed URLs, as it does now. The cache is a speed-up, never a requirement, and turning the agent off falls back cleanly.

### Authorization: the object store decides, on every hit

The cache is shared by every pod on the node, across tenants, so a hit must never be cheaper to get than a miss. **On every request, hit or miss, the agent probes the object with the pod's own pre-signed URL**, and serves the cache only if the object store answers 200. A pod that cannot read an object (no grant, a grant for another object, an expired or forged URL, a URL signed for `PUT`) gets the store's refusal and cannot learn whether the object is cached, let alone read it.

Where that URL comes from is the only difference between the two kinds of pod:

- **A grant** is redeemed at the plane's service first, which pre-signs a URL only when the grant is valid for exactly that object and method. The agent holds no S3 key and no grant key; it only relays the grant.
- **A pre-signed URL** is used as given. Whoever created the pod signed it for that one object, so it proves the same thing a redeemed grant does, and the agent skips the call to the plane's service. This is edge's path, and it needs nothing from edge beyond what it already sends.

A hit costs one round trip to the object store for a pre-signed URL, two for a grant. That is the price of not trusting what a pod names, and it is small next to a download.

### Freshness: object plus ETag

The cache key is the object and its ETag. The object is the endpoint, bucket and key, with the query string dropped, so the same object fetched through a grant, through a pre-signed URL, or through two URLs signed a minute apart is one entry. Dropping the signature from the key is safe because the signature is what the probe checks. Before using a cached file, the agent probes the object with the pre-signed URL: a ranged `GET` of one byte, which returns the current `ETag` header. (A `HEAD` would fail: the URL is signed for `GET`.) A changed object is a miss and is downloaded again; the old entry ages out.

Most artifacts never change. Edge writes each build once, under a key unique to the deployment. But the probe means correctness does not depend on that, and it costs one round trip to the object store.

Keying by content digest instead (an optional `sha256` on `download`, as the mount agent doc suggests) would dedupe identical bytes under different URLs, but it opens a hole: a pod naming its own URL with another tenant's digest would hit the other tenant's entry. Closing that needs the URL-to-digest binding checked anyway, which is what URL plus ETag already is. A `sha256` is still worth adding separately, verified before mounting, because the host kernel parses the image (see the mount agent doc). It is an integrity check, not a cache key.

### The agent only talks to services it was configured with

The agent runs on the host network, so whatever it fetches, it fetches from the node: the metadata endpoint, the kubelet, anything bound on the host. A pod-named URL must never reach anything but the object store. Three rules keep it that way:

- **The presign endpoint comes from the agent's own configuration** (one per plane, set by the chart), never from the request. A pod cannot point the agent at a server that answers with a URL of its choosing.
- **Every URL the agent fetches must be on a configured S3 endpoint**, whether a service returned it or a pod sent it: `https`, the host an exact match for a configured endpoint (no suffix match, no IP literals, no userinfo), path-style, so the bucket is in the path and the host set stays small and fixed. Edge's pre-signed URLs are already path-style (`ArtifactStore::presign`).
- **The agent follows no redirects** and refuses anything else.

A pod that sends a pre-signed URL can therefore make the agent issue one signed `GET` to the object store. That is what the pod could do itself from its own network namespace, so it gains nothing.

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
| the sidecar redeems grants, or uses its pre-signed URL, for every download | for fetch-mounts the agent does: it redeems grants at its configured endpoint, and checks pre-signed URLs against its configured S3 endpoints |
| `cmd/mount-agent`, `mountAgent.*` | renamed `cmd/node-agent`, `nodeAgent.*`, since it is no longer only a mounter. The CSI driver name stays: changing it orphans the volumes of running pods |

### What it does not change

- **Uploads, `sync` and unmounted downloads** stay in the sidecar, with grants or pre-signed URLs.
- **The Docker backend** has no agent and keeps downloading per container.
- **Isolation.** The cached image is mounted the same way an in-workspace image is today; `hostUsers: false` and gVisor work as the mount agent doc describes.

## Measuring it

The win is cold start on edge, and edge measures it already: the sidecar's per-artifact reports restore the `runtime_sidecar_*` phase timings, and `edge_runtime_duration_seconds` covers create to serving. Compare the download phase and the total by `node_pool`, cache on against off, over a day of real traffic. Also export from the agent:

- hits, misses and probe failures, by plane
- bytes served from cache against bytes downloaded
- download and probe latency
- cache size and evictions

A hit should cost one or two round trips (redeem for a grant, then probe) instead of a download.

### The hit rate on edge today is low, so it needs scheduling too

Measured on edge fra1 production on 2026-11-06, from edge's `runtime.telemetry` log line (runtime ID and node for every pod start) and ClickHouse `executions`:

- About 7,000 distinct deployments ran in 24 hours and runtime pods started about 45,000 times, over 21 runtime nodes. A cache shared by the whole region would hit about 84% of starts (1 − 7,000 / 45,000).
- Per node it would hit far less. Two nodes each started 330–360 pods in 6 hours, of 297–328 distinct deployments: only 9–15% of starts repeated a deployment that node had run in the same window. A deployment that scales to zero comes back on whichever node has room.

So the cache needs edge to send a deployment back to the same nodes. The cheapest way is a soft pin: edge ranks the runtime nodes by `hash(deployment, node)` (rendezvous hashing, so adding or removing a node moves only the deployments that ranked it in their top three) and adds a `preferredDuringSchedulingIgnoredDuringExecution` node affinity for the top three. The scheduler still places the pod anywhere when those nodes are full, so nothing gets stuck; that start is a miss, and it fills the cache on the node it lands on. Edge should leave tainted nodes (compaction) out of the ranking.

With the pin, each node holds about 3 / 21 of the region's deployments, roughly 1,000 in fra1. At the measured mean build size (functions 7.7 MB, sites 51 MB, about 21 MB per deployment) that is about 21 GB, against 42–198 GB free on the runtime nodes, so a `nodeAgent.cache.size` of 20–30 GB holds the working set. Builds above about 200 MB (the tail of sites, up to 1.3 GB) should bypass the cache.

## What it costs

- **The agent becomes a network client** of the plane services and the object store, from the host network. The two rules above are what keep that safe, and they need tests that try to break them.
- **Disk on every node**, bounded by the budget, outside kubelet's disk-pressure accounting. The agent should report its usage the same way the loop-backed workspace proposal does.
- **The agent is on the cold-start path for every mounting pod.** It already is for the mount; now it is for the download too. An agent that is down fails those pods' starts, as it does today for mounts.

## What I would do next

1. **Build it in orchestrator for jobs and revisions**, behind `nodeAgent.cache.enabled`, with kind tests that cover: a hit and a miss; a changed ETag; a pod with no grant for a cached object (refused, and it cannot tell the object is cached); an expired, forged or `PUT`-signed pre-signed URL for a cached object (refused the same way); a pre-signed URL on a host that is not a configured S3 endpoint, an IP literal, or a redirect (refused, nothing fetched); a request naming its own presign endpoint (refused); concurrent misses downloading once; eviction under a running mount.
2. **Rename the agent** in the same release, keeping the CSI driver name.
3. **Edge needs no change to use the cache.** Its runtime pods already send pre-signed URLs and already mount through the agent (edge 0.10.67 removed the privileged sidecars). Bumping the job-sidecar is enough.
4. **Edge adds the soft pin** above, behind a flag, and ships it on its own. The same `runtime.telemetry` lines measure the per-node repeat rate before and after, with or without the cache.
5. **Measure on one edge region** before turning the cache on everywhere: the repeat rate, the agent's hit rate, and the `runtime_sidecar_*` download phase by `node_pool`.
