# EROFS benchmark

This benchmark covers the three access patterns that drive the in-tree EROFS
profile: 50,000-file random access, real Node.js project startup, and a full
sequential tree traversal. It measures images through the Linux EROFS driver,
not through the Go reader.

## Reference run (2026-08-26)

The input is the production runtime assembled from Appwrite's
[`website`](https://github.com/appwrite/website) at commit
`7341ce11ca23c7c63703e9955811861b3f3f718b` after a production build. It
contains `build/`, `server/`, `src/routes/`, and production `node_modules/`:

- 172,797 filesystem entries and 159,924 regular files
- 743,375,823 logical regular-file bytes
- 133,252 regular files smaller than 1 KiB

The run used an Apple M4 Pro host and an arm64 OrbStack guest (Linux 7.0.14,
10 vCPUs, 31.5 GiB RAM), Node.js 24, and `erofs-utils` 1.9.3. Each sample calls
`sync` and writes `3` to `/proc/sys/vm/drop_caches`. Results are medians of
seven samples for random access and Node startup and five for sequential
access. Interleaving all three images limits drift; virtualized storage still
introduces occasional outliers.

| Image | Size | Build time | Random 50k | Node import | Sequential |
|---|---:|---:|---:|---:|---:|
| Previous writer, 128 KiB / 4 MiB packed extents | 295.14 MiB | 48.72 s | 802.10 ms (62.3k files/s) | 386.35 ms | 3,589.27 ms (197.5 MiB/s) |
| Optimized writer, split profile | 297.05 MiB | 46.42 s | 771.30 ms (64.8k files/s) | 386.95 ms | 2,622.90 ms (270.3 MiB/s) |
| `mkfs.erofs` 1.9.3, tuned | 284.77 MiB | 78.76 s | 735.83 ms (68.0k files/s) | 394.42 ms | 2,521.49 ms (281.2 MiB/s) |

The optimized image is 0.65% larger than the previous image, with 3.8% lower
random-access latency and 26.9% lower sequential traversal latency. Node
startup is effectively unchanged. Against the tuned upstream tool it builds
41% faster, is 4.31% larger, is within 4.8% on random access and 4.0% on
sequential traversal, and is 1.9% faster on this Node startup test.

## Profiler-guided follow-up

The initial optimized writer above was then profiled over the complete corpus
with Go CPU, allocation, scheduler, and blocking profiles. The current writer
does less work without changing its compressed-payload profile; the final
metadata-layout change also reduces image size:

- fragments feed an incremental packed-inode compressor instead of being
  written as roughly 302 MiB of raw temporary data and read back;
- files are opened lazily, so the namespace pass no longer opens and retains
  nearly 160,000 file descriptors;
- fragment, file, extent, encoder-chain, and 8 MiB segment buffers are reused
  across compression phases;
- substantial same-sized files are content-hashed before compression, so a
  whole-file duplicate reuses the canonical compressed layout instead of
  repeating both LZ4 passes and discovering the duplicate afterward;
- fragments whose size is globally unique stream directly into the packed
  tail, avoiding 194 MiB each of SHA-256 input and intermediate copying;
- the LZ4 match finder uses an inlined same-buffer match-length loop and
  rejects backward extensions whose mandatory byte already differs;
- completed compressed spans use bounded ownership-buffer pools instead of
  allocating every ordered result, and metadata serialization is pre-sized
  including deliberate NID alignment gaps;
- the strong LZ4 parse is skipped when the fast parse already occupies one
  physical block, and it stops once it cannot remove another block;
- exact incremental sequence prices replace a division in the innermost
  optimal-parse loop.

| Writer | Image bytes | Build median | Cumulative allocation |
|---|---:|---:|---:|
| Initial split-profile writer | 311,484,416 | 46.42 s | 2.23 GiB |
| Profiler-guided writer, full indexes | 308,035,584 | 30.82 s | 0.87 GiB |
| Current writer, compacted-2B indexes | 307,073,024 | 30.35 s | 0.87 GiB |

The current writer is 1.42% smaller, about 35% faster, and uses 61% less
allocation than the initial optimized writer. It is about 61% faster than
tuned `mkfs.erofs`. The final CPU profile attributes 66.9% of sampled CPU to
the strong optimal parse; the namespace walk is no longer a meaningful
build-time hot path. The image-size gap to tuned `mkfs.erofs` narrowed from
4.31% to 2.84%.

On this corpus, size-prefiltering reduced whole-file hashing to 55 candidates
and found 32 redundant files representing 70,099,961 logical bytes. Skipping
their compression reduced the preceding 36.24-second median by another 8.4%,
with exactly the same image size and no compressed read-layout change.

The follow-up module-level audit then removed another 2.39 seconds from the
build median. Its directory planner also spends metadata padding only when it
eliminates a larger external directory-data block. Across the corpus this
inlined enough small directories to remove 3,448,832 image bytes (1.11%) while
retaining the Linux 6.1-compatible format.

### Size attribution and compact indexes

A matched `mkfs.erofs` feature ablation on the same tree shows exactly where
upstream gets its size advantage. Every row uses LZ4HC level 12, a 64 KiB
pcluster, a 1 MiB decoded-extent limit, ten workers, and deterministic inode
metadata; only the named format features differ.

| `mkfs.erofs` configuration | Image bytes | Incremental effect |
|---|---:|---:|
| Legacy indexes, no fragments or dedupe | 424,734,720 | baseline |
| Compact indexes, no fragments or dedupe | 423,788,544 | -946,176 |
| Compact indexes + fragments, fragment dedupe disabled | 313,643,008 | -110,145,536 |
| Compact indexes + fragments | 302,559,232 | -11,083,776 |
| Compact indexes + fragments + global dedupe | 298,606,592 | -3,952,640 |
| Same + ztailpacking | 298,606,592 | 0 |
| Same, forcing every file into fragments | 303,321,088 | +4,714,496 |

Compact indexes account for only about 0.9 MiB of upstream's advantage, but
they are a pure metadata win. The writer now emits compacted-2B indexes whenever
its pclusters are physically contiguous and falls back to full indexes for the
few layouts where extent dedupe creates a physical jump. Compressed index bytes
fell from 1,303,352 to 335,800; the complete image fell by 962,560 bytes (0.31%)
with identical compressed payload bytes and extent budgets.

The remaining 8,466,432-byte gap to tuned `mkfs.erofs` is therefore not
ztailpacking or index overhead. On this corpus the in-tree writer packs 159,188
files whole and leaves only 137 non-empty regular files on the normal data path.
The most plausible remaining sources are upstream's different fragment-stream
parse/dedupe decisions and its `LZ4_compress_HC_destSize` fixed-output-budget
parser. Those require a genuine compression-ratio/build-CPU tradeoff; the
layout-only wins are now accounted for.

Kernel loop-device counters provide a less noisy cross-check than wall time in
the VM. Compact indexes reduce metadata work in all three target workloads,
while interleaved latency medians remain effectively flat:

| Workload | Full-index reads / sectors | Compact-index reads / sectors | Latency, full -> compact |
|---|---:|---:|---:|
| Random 50k | 8,167 / 162,744 | 8,027 / 161,624 | 693.92 -> 687.40 ms |
| Node import | 545 / 20,720 | 499 / 20,352 | 412.81 -> 409.62 ms |
| Sequential | 25,520 / 601,720 | 25,282 / 599,744 | 2,570.87 -> 2,567.25 ms |

For independent implementations, the most useful differential target is
[`rust-fs-erofs`](https://github.com/antimatter-studios/rust-fs-erofs), whose
writer has both legacy and compacted-2B indexes plus LZ4 compression. The
[`composefs-rs`](https://github.com/composefs/composefs-rs) writer and
[`Nydus`](https://github.com/dragonflyoss/nydus) RAFS v6 builder are valuable
for metadata-only and chunk-addressed EROFS layout ideas, but are not direct
compressed-image size competitors. Outside EROFS, `mksquashfs` and
`gensquashfs` are useful format-level baselines for the same mounted workloads;
their results should be kept separate because their metadata and decompression
semantics differ.

The packed-inode phase uses up to 12 workers because its 1 MiB scratch is
cheap, while normal 4 MiB extents remain capped at eight workers. On this
machine that recovered another 4% of build time without materially changing
peak RSS; using 12 workers for both phases reached roughly 35.2 seconds but
needed another 55–65 MiB.

The physical format and extent budgets are unchanged. A fresh interleaved
kernel-mounted A/B run confirms that the work elimination did not buy build
speed at read-time expense (medians; virtualized storage had large outliers):

| Image | Random 50k | Node import | Sequential |
|---|---:|---:|---:|
| Initial split-profile writer | 832.08 ms | 390.43 ms | 2,641.47 ms |
| Current profiler-guided writer | 825.66 ms | 390.89 ms | 2,527.56 ms |
| `mkfs.erofs` 1.9.3, tuned | 744.90 ms | 396.65 ms | 2,630.91 ms |

A separate interleaved A/B isolates the aligned-inline layout change. The VM
occasionally produced multi-second storage outliers, so loop-device counters
were recorded as a deterministic cross-check:

| Layout | Random 50k | Node import | Sequential |
|---|---:|---:|---:|
| Previous metadata layout | 771.21 ms | 373.57 ms | 2,758.31 ms |
| Pareto-aligned inline dirs | 718.33 ms | 375.72 ms | 2,462.87 ms |

Node time is effectively flat, but physical reads fell from 589 to 545
(7.3%). Random-access reads fell from 8,948 to 8,167 (8.7%) and sectors from
168,992 to 162,744 (3.7%), confirming that the latency improvements come from
less kernel I/O rather than a CPU tradeoff.

### Node startup profile

A cold `perf` profile of the Node import attributes 69.1% of samples to Node
and V8, 23.2% to the kernel as a whole, and only 2.6% to LZ4 decompression.
`strace` records 19,031 `statx` calls, 2,182 `openat` calls, and 1,870 unique
project files opened during startup. Decoder SIMD therefore has only about a
10 ms theoretical ceiling on this workload.

The writer accepts an optional ordered hot-path manifest through
`CompressionOptions.FragmentOrder`; the benchmark helper exposes it as
`-fragment-order`. Packing the 1,870 traced paths first reduced the Node median
from 374.87 to 362.95 ms (3.2%), but grew this image by 372,736 bytes (0.12%).
It remains an opt-in PGO facility rather than the default profile.

Two additional prototypes were rejected. Reordering only the physical packed
extents preserved image size but made Node startup about 2.6% slower because
logical readahead became physically scattered. Building one dense immutable
LZ4 match chain for all prefix probes reduced image size by 958,464 bytes
(0.31%), but increased build time from 36.24 to 51.71 seconds: denser chains
made every bounded match search examine more candidates. Neither is enabled.

The tuned upstream image was built with:

```sh
mkfs.erofs --quiet -zlz4hc,level=12 -C65536 \
  --max-extent-bytes=1048576 \
  -Efragments=65536,ztailpacking,dedupe,force-inode-compact \
  --workers=10 -T0 -Uclear IMAGE SOURCE
```

That deliberately uses current upstream size and locality features. The
in-tree writer uses whole-file fragment packing and compacted-2B indexes where
possible. The matched ablation above shows that tail packing is inactive for
this profile and that upstream's remaining advantage is in payload encoding.

## Helpers

Build the image generator for the benchmark machine:

```sh
go build -o mkimage ./hack/erofs-bench/mkimage
./mkimage -workers 10 SOURCE optimized.erofs

# Optional: place a traced startup working set first in the packed inode.
./mkimage -workers 12 -fragment-order node-hot-paths.txt \
  SOURCE startup-pgo.erofs
```

Capture a whole-image CPU profile, final heap profile, and execution trace
with the same helper:

```sh
./mkimage -workers 10 \
  -cpuprofile build.cpu -memprofile build.mem -trace build.trace \
  SOURCE optimized.erofs

go tool pprof -top ./mkimage build.cpu
go tool pprof -top -alloc_space ./mkimage build.mem
go tool trace build.trace
```

Build the read harness for Linux, mount an image, and run either workload as
root (dropping caches requires privilege):

```sh
go build -o readtree ./hack/erofs-bench/readtree
mount -t erofs -o loop,ro optimized.erofs /mnt/erofs

./readtree -root /mnt/erofs -source SOURCE -mode random \
  -limit 50000 -read-size 4096 -seed 7 -drop-caches

./readtree -root /mnt/erofs -source SOURCE -mode sequential \
  -limit 0 -drop-caches
```

`-source` makes the harness enumerate the same paths from the unmounted tree
before dropping caches, keeping directory enumeration out of the timed region.
The Node startup sample imports the built SvelteKit handler:

```sh
sync && echo 3 > /proc/sys/vm/drop_caches
cd /mnt/erofs
node --input-type=module -e \
  'const t=performance.now(); await import("./build/handler.js"); console.log(performance.now()-t)'
```
