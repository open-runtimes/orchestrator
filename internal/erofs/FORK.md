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
  dedupe of pclusters and fragments. Verified against `fsck.erofs`,
  `dump.erofs`, and Linux kernel mounts.
- **Compressed read**: `Open` handles the same subset (full indexes, lz4,
  fragments), including images produced by `mkfs.erofs -E legacy-compress`.
  Compact (COMPRESSED_COMPACT) indexes and other algorithms remain
  unsupported.

Import paths are rewritten to `orchestrator/internal/erofs`, and the upstream
test harness (which shells out to a local `mkfs.erofs` binary) is dropped in
favor of self-contained round-trip tests.
