package artifact

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// squashfsHeader builds a minimal superblock carrying the given compression id.
func squashfsHeader(compressionID uint16) []byte {
	b := make([]byte, 96)
	copy(b, squashfsMagic)
	binary.LittleEndian.PutUint16(b[squashfsCompressionIDOffset:], compressionID)

	return b
}

func erofsHeader() []byte {
	b := make([]byte, erofsMagicOffset+len(erofsMagic))
	copy(b[erofsMagicOffset:], erofsMagic)

	return b
}

func tarHeader() []byte {
	b := make([]byte, 512)
	copy(b[257:], "ustar")

	return b
}

func TestClassifySeparatesFormatFromCompression(t *testing.T) {
	tests := []struct {
		name        string
		header      []byte
		format      string
		compression string
	}{
		// The point of the split: every one of these is a .sqfs on disk, and
		// only the superblock says which codec is inside.
		{"squashfs gzip", squashfsHeader(1), "squashfs", "gzip"},
		{"squashfs lzma", squashfsHeader(2), "squashfs", "lzma"},
		{"squashfs lzo", squashfsHeader(3), "squashfs", "lzo"},
		{"squashfs xz", squashfsHeader(4), "squashfs", "xz"},
		{"squashfs lz4", squashfsHeader(5), "squashfs", "lz4"},
		{"squashfs zstd", squashfsHeader(6), "squashfs", "zstd"},
		{"squashfs unknown codec id", squashfsHeader(99), "squashfs", ""},
		{"erofs", erofsHeader(), "erofs", ""},
		{"tar gzip", []byte("\x1f\x8b\x08\x00"), "tar", "gzip"},
		{"tar zstd", []byte("\x28\xb5\x2f\xfd"), "tar", "zstd"},
		{"tar lz4", []byte("\x04\x22\x4d\x18"), "tar", "lz4"},
		{"plain tar", tarHeader(), "tar", "none"},
		{"unrecognized", []byte("not an archive at all"), "", ""},
		{"empty", nil, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			format, compression := Classify(tc.header)
			if format != tc.format || compression != tc.compression {
				t.Errorf("Classify() = (%q, %q), want (%q, %q)", format, compression, tc.format, tc.compression)
			}
		})
	}
}

// A truncated squashfs header must not be reported as gzip: gzip is the format
// default, so guessing it would silently mislabel every image built otherwise.
func TestClassifyLeavesTruncatedSquashfsCodecEmpty(t *testing.T) {
	format, compression := Classify([]byte(squashfsMagic))
	if format != "squashfs" {
		t.Errorf("format = %q, want squashfs", format)
	}
	if compression != "" {
		t.Errorf("compression = %q, want empty for a truncated header", compression)
	}
}

func TestClassifyFileReadsHeaderFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.sqfs")
	if err := os.WriteFile(path, squashfsHeader(5), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	format, compression, err := ClassifyFile(path)
	if err != nil {
		t.Fatalf("ClassifyFile() error = %v", err)
	}
	if format != "squashfs" || compression != "lz4" {
		t.Errorf("ClassifyFile() = (%q, %q), want (squashfs, lz4)", format, compression)
	}
}

// A file shorter than the sniff window is normal, not an error.
func TestClassifyFileToleratesShortFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.tar.gz")
	if err := os.WriteFile(path, []byte("\x1f\x8b\x08\x00"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	format, compression, err := ClassifyFile(path)
	if err != nil {
		t.Fatalf("ClassifyFile() error = %v", err)
	}
	if format != "tar" || compression != "gzip" {
		t.Errorf("ClassifyFile() = (%q, %q), want (tar, gzip)", format, compression)
	}
}

func TestClassifyFileReturnsErrorForMissingFile(t *testing.T) {
	if _, _, err := ClassifyFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("ClassifyFile() error = nil, want an error for a missing file")
	}
}
