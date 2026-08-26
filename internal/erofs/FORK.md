# internal/erofs — vendored fork

Fork of [`github.com/erofs/go-erofs`](https://github.com/erofs/go-erofs)
`v0.3.0`, Apache-2.0 licensed (see `LICENSE`).

## Why fork

Upstream reads and writes uncompressed EROFS only — compressed inodes are an
open roadmap item there. Orchestrator artifacts need z_erofs compressed
images (small archives that stay fast under random small-file reads), so this
fork adds, in `compress.go` and `zread.go`:

- **Compressed write**: `WithCompression` produces COMPRESSED_FULL
  (non-compact index) inodes with lz4 or lz4hc 0-padded (big) pclusters,
  optional packed-inode fragments for whole small files, and content-hash
  dedupe of pclusters and fragments. Data and the shared packed inode can use
  independent physical-cluster and decoded-extent limits, bounding small-file
  read amplification without giving up large-file compression. Compressed
  images place metadata and directory blocks before payload data for cold path
  lookup locality. Small-file input is fed directly into a bounded parallel
  packed-inode pipeline, avoiding a complete raw temporary copy; source files
  are opened only when a compressor is ready for them. Encoder state, extent
  scratch, input buffers, and 8 MiB stream segments are reused across phases,
  keeping cumulative allocation and garbage collection independent of the
  number of files. Substantial same-sized files are hashed before compression,
  allowing whole-file duplicates to share the canonical compressed layout
  without repeating the expensive encoder passes. An optional hot-path
  manifest can profile-guide packed fragment order for startup-sensitive
  images. Metadata planning pads a small directory inode to the next block
  only when inlining it removes more external data, reducing both image bytes
  and lookup I/O. Verified against `fsck.erofs`, `dump.erofs`, and Linux kernel
  mounts.
- **Compressed read**: `Open` handles the same subset (full indexes, lz4,
  fragments), including images produced by `mkfs.erofs -E legacy-compress`.
  It caches parsed packed-inode state and complete decompressed extents, so a
  sequential read does not repeat index walks, physical reads, or decompression
  for every logical block. Compact (COMPRESSED_COMPACT) indexes and other
  algorithms remain unsupported.

The artifact profile uses 128 KiB / 4 MiB physical/decoded limits for normal
files and 64 KiB / 1 MiB for the packed inode. See
[`hack/erofs-bench/README.md`](../../hack/erofs-bench/README.md) for the
workload, reproducible helpers, and comparison with `mkfs.erofs` 1.9.3.

Import paths are rewritten to `orchestrator/internal/erofs`, and the upstream
test harness (which shells out to a local `mkfs.erofs` binary) is dropped in
favor of self-contained round-trip tests.
