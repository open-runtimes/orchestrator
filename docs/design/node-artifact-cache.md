# Node artifact cache

**Status: built**, behind `mountAgent.cache.endpoints` (off by default). It builds on the [node mount agent](node-mount-agent.md). A pod proves it may read an object with a pre-signed URL: one its creator signed, which is what edge does, or one the sidecar gets by redeeming a per-transfer S3 grant (`internal/artifact/grant.go`, [jobs: S3 URLs](../jobs.md#s3-urls)).

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

When the agent has a cache, it also fetches. A download that feeds a mount is no longer done by the sidecar into the workspace. The sidecar turns the download's URL into a pre-signed GET (an `s3://` URL by redeeming its grant, as it would for any download; any other URL is one already) and hands that to the agent. The agent fetches the object once per node into a node-local cache and loop-mounts the cached file read-only at `out`. With `writable`, the overlay and its upper are the pod's, as today.

```
mount phase ─► sidecar: s3:// + grant ─► redeem at the plane's service ─► pre-signed GET URL
                        pre-signed URL ─► as given
            ─► sidecar: POST /v1/fetch-mount {url, target, writable, size}
            ─► agent:   URL on a configured endpoint? ─► probe it (ranged GET, 1 byte) ─► ETag, size
            ─► hit:     cache has (object, ETag) ─► loop-mount the cached file
            ─► miss:    download once (concurrent misses wait on one download) ─► then mount
            ─► refused: the sidecar downloads and mounts it itself, as without a cache
```

The agent only ever sees pre-signed URLs. It holds no S3 key, no grant and no grant key, and it calls no plane service.

The artifact API does not change. A `download` whose only reader is one `mount` of its output is what triggers the agent path; the runner recognises the pair (`sidecar.cachedDownloads`). A download anything else reads (an archive to unpack, a file the workload reads, a second mount), one with headers of its own, and uploads and mount `sync`, which are per-pod writes, keep going through the sidecar.

Without a cache, nothing changes: the agent has no `/v1/fetch-mount`, answers 404, and the sidecar downloads as it does now. So does any refusal: a host the agent does not fetch from, an object over the size limit, a tar build the kernel cannot mount, a failed probe. The cache is a speed-up, never a requirement. A job pod's pre phase runs in a container with no agent token, so it downloads its images as before, and the post sidecar mounts them from the workspace; only combined-mode sidecars (edge's runtimes) use the cache today.

### Authorization: the object store decides, on every hit

The cache is shared by every pod on the node, across tenants, so a hit must never be cheaper to get than a miss. **On every request, hit or miss, the agent probes the object with the pod's own pre-signed URL**, and serves the cache only if the object store answers 200. A pod that cannot read an object (no grant, a grant for another object, an expired or forged URL, a URL signed for `PUT`) gets the store's refusal and cannot learn whether the object is cached, let alone read it.

A hit costs one round trip to the object store, plus the grant redemption the sidecar already does for an `s3://` URL. That is the price of not trusting what a pod names, and it is small next to a download.

### Freshness: object plus ETag

The cache key is the object and its ETag. The object is the endpoint, bucket and key, with the query string dropped, so the same object fetched through a grant, through a pre-signed URL, or through two URLs signed a minute apart is one entry. Dropping the signature from the key is safe because the signature is what the probe checks. Before using a cached file, the agent probes the object with the pre-signed URL: a ranged `GET` of one byte, which returns the current `ETag` header. (A `HEAD` would fail: the URL is signed for `GET`.) A changed object is a miss and is downloaded again; the old entry ages out.

Most artifacts never change. Edge writes each build once, under a key unique to the deployment. But the probe means correctness does not depend on that, and it costs one round trip to the object store.

Keying by content digest instead (an optional `sha256` on `download`, as the mount agent doc suggests) would dedupe identical bytes under different URLs, but it opens a hole: a pod naming its own URL with another tenant's digest would hit the other tenant's entry. Closing that needs the URL-to-digest binding checked anyway, which is what URL plus ETag already is. A `sha256` is still worth adding separately, verified before mounting, because the host kernel parses the image (see the mount agent doc). It is an integrity check, not a cache key.

### The agent only talks to services it was configured with

The agent runs on the host network, so whatever it fetches, it fetches from the node: the metadata endpoint, the kubelet, anything bound on the host. A pod-named URL must never reach anything but the object store. Two rules keep it that way:

- **Every URL the agent fetches must be on a configured endpoint** (`mountAgent.cache.endpoints`): the same scheme, the host and port an exact match (no suffix match), no userinfo. A configured endpoint is a hostname such as `fra1.digitaloceanspaces.com`, so an IP literal never matches. Edge's pre-signed URLs are path-style (`ArtifactStore::presign`), so the bucket is in the path and the host set stays small and fixed.
- **The agent follows no redirects.**

A pod that sends a pre-signed URL can therefore make the agent issue one signed `GET` to the object store. That is what the pod could do itself from its own network namespace, so it gains nothing.

This replaces nothing in the existing per-pod token check: the token still selects the workspace the mount lands in, exactly as for `/v1/mount`.

### Storage, eviction and concurrency

- **Where:** `/var/lib/kubelet/plugins/mount.open-runtimes.dev/cache` on the node's disk, beside the workspaces.
- **Budget:** `mountAgent.cache.sizeGiB` (20), least recently used out first; a hit refreshes the file's mtime, which eviction sorts by. Eviction unlinks the file. A file still loop-mounted by a running pod keeps its inode until the last mount goes, so eviction never breaks a pod; the disk comes back when that pod ends.
- **Size limit:** objects over `mountAgent.cache.maxObjectMiB` (200) are not cached, from the size the probe reports. The tail of large sites keeps downloading per pod.
- **Read-only, never hard-linked into a workspace.** The workspace is attacker-writable. A hard link would let one pod rewrite a file other pods mount. The loop device is opened read-only on the cache file, and the writable layer is the pod's own tmpfs upper.
- **One download per key.** Concurrent misses for the same key wait on a single in-flight download. A burst of replicas on one node downloads once. The download is detached from the request that started it and bounded at 5 minutes.
- **Partial files never enter the cache.** Download to a temporary name, `fsync`, rename into place. A changed ETag between the probe and the download is a refusal, not an entry.
- **Restart:** an entry's name is a hash of its key, so an agent restart keeps every entry; half-written downloads are removed on start.

### What moves where in the code

| Today | With the cache |
| --- | --- |
| `sidecar.Runner` runs `download`, then `mount` through `mountagent.Client` | a `download`→`mount` pair goes to the agent as one `/v1/fetch-mount`; everything else is unchanged |
| the agent mounts a workspace file | it can also mount a cache file it opened (`mount.Beneath.MountImage`) |
| the sidecar redeems grants for every `s3://` download | unchanged: for a fetch-mount it redeems the grant and sends the agent the URL it got back |

The agent keeps its name (`cmd/mount-agent`, `mountAgent.*`). Renaming it buys nothing a reader needs and the CSI driver name could not change anyway.

### What it does not change

- **Uploads, `sync` and unmounted downloads** stay in the sidecar, with grants or pre-signed URLs.
- **Job pods.** Their pre container has no agent token, by design, and downloads before the post sidecar mounts.
- **The Docker backend** has no agent and keeps downloading per container.
- **Isolation.** The cached image is mounted the same way an in-workspace image is today; `hostUsers: false` and gVisor work as the mount agent doc describes.

## Measuring it

The win is cold start on edge, and edge measures it already: the sidecar's per-artifact reports restore the `runtime_sidecar_*` phase timings, and `edge_runtime_duration_seconds` covers create to serving. Compare the download phase and the total by `node_pool`, cache on against off, over a day of real traffic.

The cache reports through logs for now, which is enough to decide whether it pays:

- the sidecar logs `Mounted image from the node cache` with `hit`, or `Node cache did not serve the image, downloading it here` with the reason;
- the agent logs `Cached image` with the object, bytes and duration of each download, and each fetch-mount's duration like any other mount request.

A hit should cost one probe instead of a download.

### The hit rate on edge today is low, so it needs scheduling too

Measured on edge fra1 production on 2026-11-06, from edge's `runtime.telemetry` log line (runtime ID and node for every pod start) and ClickHouse `executions`:

- About 7,000 distinct deployments ran in 24 hours and runtime pods started about 45,000 times, over 21 runtime nodes. A cache shared by the whole region would hit about 84% of starts (1 − 7,000 / 45,000).
- Per node it would hit far less. Two nodes each started 330–360 pods in 6 hours, of 297–328 distinct deployments: only 9–15% of starts repeated a deployment that node had run in the same window. A deployment that scales to zero comes back on whichever node has room.

So the cache needs edge to send a deployment back to the same nodes. The cheapest way is a soft pin: edge ranks the runtime nodes by `hash(deployment, node)` (rendezvous hashing, so adding or removing a node moves only the deployments that ranked it in their top three) and adds a `preferredDuringSchedulingIgnoredDuringExecution` node affinity for the top three. The scheduler still places the pod anywhere when those nodes are full, so nothing gets stuck; that start is a miss, and it fills the cache on the node it lands on. Tainted nodes (compaction) are left out of the ranking. Edge shipped this as `_EDGE_RUNTIME_PIN_NODES=3` (edge#1823, all production regions since 2026-11-10); 2.5 days later fra1 nodes repeated 35–60% of their starts within 6 hours, against 71% for a regional cache in the same window.

With the pin, each node holds about 3 / 21 of the region's deployments, roughly 1,000 in fra1. At the measured mean build size (functions 7.7 MB, sites 51 MB, about 21 MB per deployment) that is about 21 GB, against 42–198 GB free on the runtime nodes, so the default `mountAgent.cache.sizeGiB` of 20 holds the working set. Builds above `maxObjectMiB` (200; the tail of sites, up to 1.3 GB) bypass the cache.

## What it costs

- **The agent becomes a network client** of the object store, from the host network. The two rules above are what keep that safe; `internal/mountagent/node/cache_test.go` tries to break each of them.
- **Disk on every node**, bounded by the budget, outside kubelet's disk-pressure accounting. The agent should report its usage the same way the loop-backed workspace proposal does.
- **The agent is on the cold-start path for every mounting pod.** It already is for the mount; now it is for the download too. An agent that is down fails those pods' starts, as it does today for mounts.


## Rollout

1. **One staging edge region:** set `mountAgent.cache.endpoints` to its Spaces endpoint and bump the job-sidecar. Edge needs no change: its runtime pods send pre-signed URLs and mount through the agent, and its soft pin (edge#1823) already sends a runtime back to the same three nodes.
2. **One production region**, then measure there: the agent's hit rate, the `runtime_sidecar_*` download phase by `node_pool`, and cache disk use. With the pin, fra1 nodes repeat about half their starts (35–60%, 2026-11-12), against a regional ceiling of about 71%.
3. **Then everywhere**, and gVisor templates after it (a second kind of entry, keyed on the cached image they came from).
