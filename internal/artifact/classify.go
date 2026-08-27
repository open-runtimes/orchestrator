package artifact

import (
	"errors"
	"io"
	"os"
)

// classifyHeaderBytes is enough to cover every magic Classify checks. squashfs
// sits at offset 0 and tar's "ustar" at 257, but erofs keeps its superblock
// magic at 1024 — so read past that rather than the 512 the other two need.
const classifyHeaderBytes = 1028

// Classify names what an artifact actually is, from its header rather than its
// filename.
//
// Format and compression are two axes, not one value. "squashfs" and "gzip"
// answer different questions — what holds the files, and what codec sits
// inside — and a name like code.sqfs reveals neither: every squashfs image is
// named that way whether it was built with lz4 or zstd, and only the
// superblock records which.
//
// Returns empty strings when the header matches nothing known, so callers can
// tell "not recognized" from a real format.
func Classify(header []byte) (format, compression string) {
	switch {
	case isSquashfs(header):
		// Codec may still be "" for a truncated header or an id this build
		// does not know; reporting squashfs with an unknown codec beats
		// claiming a default it was not built with.
		return "squashfs", squashfsCodec(header)
	case isErofs(header):
		return "erofs", ""
	case isGzip(header):
		return "tar", "gzip"
	case isZstd(header):
		return "tar", "zstd"
	case isLz4(header):
		return "tar", "lz4"
	case isTar(header):
		return "tar", "none"
	default:
		return "", ""
	}
}

// ClassifyFile reads just the header of the file at path and classifies it.
//
// A short read is expected — a small archive is legitimately shorter than the
// header — and yields whatever the available bytes identify. Any other read
// error is returned, so a filesystem failure is never reported as an
// unrecognized format.
func ClassifyFile(path string) (format, compression string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	header := make([]byte, classifyHeaderBytes)
	n, err := io.ReadFull(f, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", "", err
	}

	format, compression = Classify(header[:n])

	return format, compression, nil
}
