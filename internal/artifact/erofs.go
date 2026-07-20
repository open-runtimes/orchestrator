package artifact

import (
	"bytes"
	"fmt"
	"os"

	erofs "github.com/erofs/go-erofs"
)

// erofs writes its superblock magic at a fixed offset rather than the start of
// the image, so callers must sniff at least erofsMagicOffset+4 bytes before
// checking. The magic is little-endian 0xE0F5E1E2.
const erofsMagicOffset = 1024

var erofsMagic = []byte{0xe2, 0xe1, 0xf5, 0xe0}

// isErofs reports whether b holds the erofs superblock magic at offset 1024.
func isErofs(b []byte) bool {
	return len(b) >= erofsMagicOffset+4 && bytes.Equal(b[erofsMagicOffset:erofsMagicOffset+4], erofsMagic)
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
