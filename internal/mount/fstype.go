package mount

import (
	"errors"
	"io"
)

// FsType sniffs the on-disk magic to choose the kernel filesystem type for an
// image: squashfs ("hsqs" at offset 0) or erofs (0xE0F5E1E2, little-endian, at
// offset 1024). Both are read-only image formats loop-mounted identically
// apart from the type string. Reading at offsets leaves a file's position
// untouched, so the same descriptor can then back the loop device.
func FsType(r io.ReaderAt) (string, error) {
	// erofs's magic sits at offset 1024, so read past it.
	buf := make([]byte, 1028)
	n, _ := r.ReadAt(buf, 0)
	buf = buf[:n]

	switch {
	case len(buf) >= 4 && string(buf[:4]) == "hsqs":
		return "squashfs", nil
	case len(buf) >= 1028 && buf[1024] == 0xe2 && buf[1025] == 0xe1 && buf[1026] == 0xf5 && buf[1027] == 0xe0:
		return "erofs", nil
	default:
		return "", errors.New("unrecognized filesystem image (not squashfs or erofs)")
	}
}
