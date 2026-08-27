package artifact

import (
	"fmt"
	"os"

	"orchestrator/internal/erofs"
)

// erofs writes its superblock magic at a fixed offset rather than the start of
// the image, so callers must sniff at least erofsMagicOffset+len(erofsMagic)
// bytes before checking. The magic is little-endian 0xE0F5E1E2.
const (
	erofsMagicOffset = 1024
	erofsMagic       = "\xe2\xe1\xf5\xe0"
)

// isErofs reports whether b holds the erofs superblock magic at offset 1024.
func isErofs(b []byte) bool {
	end := erofsMagicOffset + len(erofsMagic)
	return len(b) >= end && string(b[erofsMagicOffset:end]) == erofsMagic
}

// erofsCompression validates an erofs compression name. ok is false for an
// empty/none name (uncompressed image); lz4 and lz4hc produce z_erofs
// compressed images with fragments and dedupe enabled — the configuration
// that wins on both artifact size and cold-read speed for build outputs full
// of small files.
func erofsCompression(name string) (opts erofs.CompressionOptions, ok bool, err error) {
	switch name {
	case "", "none":
		return erofs.CompressionOptions{}, false, nil
	case "lz4", "lz4hc":
		return erofs.CompressionOptions{
			Algorithm:           name,
			PClusterSize:        128 << 10,
			MaxExtentSize:       4 << 20,
			Fragments:           true,
			PackedPClusterSize:  64 << 10,
			PackedMaxExtentSize: 1 << 20,
			Dedupe:              true,
		}, true, nil
	default:
		return erofs.CompressionOptions{}, false, fmt.Errorf("unsupported erofs compression: %q (supported: lz4, lz4hc, none)", name)
	}
}

// writeErofs builds an erofs image at destPath from the file or directory at
// srcPath, uncompressed or z_erofs lz4/lz4hc compressed. The kernel erofs
// driver and the reader mount and extract both forms directly.
func writeErofs(srcPath, destPath, compression string) error {
	comp, compressed, err := erofsCompression(compression)
	if err != nil {
		return err
	}

	src, err := sourceFS(srcPath)
	if err != nil {
		return err
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer out.Close()

	// CopyFrom streams each entry from src; sourceFS wraps a single file so it
	// streams too rather than buffering it (symmetric with writeSquashfs).
	var opts []erofs.CreateOpt
	if compressed {
		// Compact inodes drop per-file mtimes (reads report the build time),
		// matching mkfs.erofs defaults — irrelevant for build artifacts and
		// roughly half the per-inode metadata on small-file-heavy trees.
		opts = append(opts, erofs.WithCompression(comp), erofs.WithCompactInodes())
	}
	w := erofs.Create(out, opts...)
	if err := w.CopyFrom(src); err != nil {
		return fmt.Errorf("failed to add source: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("failed to finalize erofs: %w", err)
	}
	return nil
}

// extractErofs extracts an erofs image at srcPath into destDir. See extractFS
// for how strip and subdir are applied. (This is the `unarchive` path; the
// `mount` artifact mounts the image read-only instead of materializing it.)
func extractErofs(srcPath, destDir, subdir string, strip bool) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open erofs: %w", err)
	}
	defer f.Close()

	img, err := erofs.Open(f)
	if err != nil {
		return fmt.Errorf("failed to open erofs: %w", err)
	}
	return extractFS(img, destDir, subdir, strip)
}
