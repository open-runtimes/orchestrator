# internal/squashfs — vendored fork

Fork of [`github.com/KarpelesLab/squashfs`](https://github.com/KarpelesLab/squashfs)
`v1.2.0`, MIT-licensed (see `LICENSE`).

## Why fork

Upstream defines the `LZ4` compression ID but never implements lz4 writing: it
neither emits the compressor-options superblock record that lz4 mandates nor
lets a caller set the `COMPRESSOR_OPTIONS` flag. Without that record the kernel
squashfs driver and `unsquashfs` reject the image — so a library-only lz4 image
could not be mounted (the whole point of the `mount` artifact) or unpacked by
standard tools. There is no public hook to add it, so we vendor and patch.

## Changes from upstream

- **`writer.go`** — `Finalize` writes a 10-byte lz4 compressor-options metadata
  block right after the superblock (`writeCompressorOptions`), and
  `buildSuperblock` sets the `COMPRESSOR_OPTIONS` flag, both only when the
  compressor is `LZ4`. Search for `LZ4` in that file.
- **Removed** the FUSE mount support (`inode_fuse.go`, `inode_linux.go`,
  `inode_darwin.go`, all `//go:build fuse`-tagged) to drop the `go-fuse`
  dependency. We mount via the kernel, never FUSE.
- **Removed** `comp_zstd.go` and `comp_xz.go` — build-tag-gated codec
  registrations that never compile in our builds (we register zstd and lz4
  ourselves, and don't use xz). Dropping `comp_xz.go` also sheds the
  `ulikunitz/xz` dependency. gzip stays (it lives in `comp.go`, always on).

The lz4 *block* codec (raw `lz4.CompressBlock`/`UncompressBlock`, not the
self-framed stream format) is registered by the parent package in
`internal/artifact/squashfs.go`, not here.

## Re-syncing with upstream

Re-copy the upstream `.go` files (minus tests and the FUSE files above), then
re-apply the `LZ4` edits in `writer.go`. The patch is small and self-contained.
