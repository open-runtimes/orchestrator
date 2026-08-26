package artifact

import (
	"fmt"
	"os"

	erofs "orchestrator/internal/erofs"
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

// writeErofs builds an erofs image at destPath from the file or directory at
// srcPath. Unlike squashfs, the go-erofs writer exposes no compression or block
// size options, so the image is always stored uncompressed — hence writeErofs
// takes no compression/blockSize arguments. The kernel erofs driver and the
// reader still mount and extract it directly.
func writeErofs(srcPath, destPath string) error {
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
	w := erofs.Create(out)
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
