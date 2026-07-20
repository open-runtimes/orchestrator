package artifact

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"orchestrator/internal/squashfs"
)

// archiveSquashfs builds a squashfs image from a small tree and returns its path.
func archiveSquashfs(t *testing.T, compression string) string {
	t.Helper()
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "out.sqfs", Format: "squashfs", Compression: compression}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}
	return filepath.Join(tmpDir, "out.sqfs")
}

// assertSquashfsContents opens the image (test-only; production mounts, never
// extracts) and verifies the expected files round-tripped.
func assertSquashfsContents(t *testing.T, image string) {
	t.Helper()
	sb, err := squashfs.Open(image)
	if err != nil {
		t.Fatalf("squashfs.Open() error = %v", err)
	}
	defer sb.Close()

	if got, err := fs.ReadFile(sb, "file.txt"); err != nil || string(got) != "hello" {
		t.Fatalf("file.txt = %q, err = %v", got, err)
	}
	if got, err := fs.ReadFile(sb, "sub/nested.txt"); err != nil || string(got) != "nested" {
		t.Fatalf("sub/nested.txt = %q, err = %v", got, err)
	}
}

func TestArchive_Squashfs_Compressors(t *testing.T) {
	for _, comp := range []string{"", "gzip", "zstd", "lz4"} {
		t.Run("comp="+comp, func(t *testing.T) {
			image := archiveSquashfs(t, comp)

			magic, err := os.ReadFile(image)
			if err != nil || !isSquashfs(magic) {
				t.Fatalf("output is not a squashfs image (err=%v)", err)
			}
			assertSquashfsContents(t, image)
		})
	}
}

// TestArchive_Squashfs_SingleFile covers the single-file path (streamed via a
// one-entry fs.FS, not buffered into memory): the file lands at the image root.
func TestArchive_Squashfs_SingleFile(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "solo.txt"), []byte("solo content"), 0o644)

	arc := &Archive{ID: "a", In: "solo.txt", Out: "out.sqfs", Format: "squashfs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	sb, err := squashfs.Open(filepath.Join(tmpDir, "out.sqfs"))
	if err != nil {
		t.Fatalf("squashfs.Open() error = %v", err)
	}
	defer sb.Close()
	if got, err := fs.ReadFile(sb, "solo.txt"); err != nil || string(got) != "solo content" {
		t.Fatalf("solo.txt = %q, err = %v", got, err)
	}
}

func TestArchive_Squashfs_InvalidCompression(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "src"), 0o755)

	arc := &Archive{ID: "a", In: "src", Out: "out.sqfs", Format: "squashfs", Compression: "brotli"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error == nil {
		t.Error("expected error for unsupported compression")
	}
}

// TestArchive_Squashfs_BlockSize confirms the block size recorded in the
// superblock: an unset size defaults to 1 MiB (matching mksquashfs -b 1M), and
// an explicit size is honored.
func TestArchive_Squashfs_BlockSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  int
		want uint32
	}{
		{"default is 1 MiB", 0, 1 << 20},
		{"explicit 128 KiB", 128 << 10, 128 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			os.MkdirAll(srcDir, 0o755)
			os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)

			arc := &Archive{ID: "a", In: "src", Out: "out.sqfs", Format: "squashfs", BlockSize: tc.set}
			if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
				t.Fatalf("archive Apply() error = %v", r.Error)
			}

			sb, err := squashfs.Open(filepath.Join(tmpDir, "out.sqfs"))
			if err != nil {
				t.Fatalf("squashfs.Open() error = %v", err)
			}
			defer sb.Close()
			if sb.BlockSize != tc.want {
				t.Fatalf("BlockSize = %d, want %d", sb.BlockSize, tc.want)
			}
		})
	}
}

// TestUnarchive_ExtractsSquashfs round-trips a squashfs image back out through
// unarchive (the materializing path, vs. the mount artifact).
func TestUnarchive_ExtractsSquashfs(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.sqfs", Format: "squashfs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.sqfs", Out: "extracted"}
	if r := un.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("unarchive Apply() error = %v", r.Error)
	}

	if got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "file.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("file.txt = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "sub", "nested.txt")); err != nil || string(got) != "nested" {
		t.Fatalf("sub/nested.txt = %q, err = %v", got, err)
	}
}

// TestUnarchive_ExtractsSquashfs_Strip drops the single wrapper directory.
func TestUnarchive_ExtractsSquashfs_Strip(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "repo", "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "repo", "file.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "repo", "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.sqfs", Format: "squashfs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.sqfs", Out: "extracted", Strip: true}
	if r := un.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("unarchive Apply() error = %v", r.Error)
	}

	if got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "file.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("file.txt = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "sub", "nested.txt")); err != nil || string(got) != "nested" {
		t.Fatalf("sub/nested.txt = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "extracted", "repo")); !os.IsNotExist(err) {
		t.Fatal("wrapper root directory should not exist in extracted directory")
	}
}

// TestUnarchive_ExtractsSquashfs_Strip_Flat fails loudly when strip is used on
// an image with no wrapper directory, instead of extracting nothing.
func TestUnarchive_ExtractsSquashfs_Strip_Flat(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.sqfs", Format: "squashfs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.sqfs", Out: "extracted", Strip: true}
	if r := un.Apply(t.Context(), tmpDir); r.Error == nil {
		t.Fatal("expected error for strip on a flat image, got success")
	}
}

// TestUnarchive_ExtractsSquashfs_Subdir extracts only a subtree.
func TestUnarchive_ExtractsSquashfs_Subdir(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.sqfs", Format: "squashfs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.sqfs", Out: "extracted", Subdir: "sub"}
	if r := un.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("unarchive Apply() error = %v", r.Error)
	}

	if got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "nested.txt")); err != nil || string(got) != "nested" {
		t.Fatalf("nested.txt = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "extracted", "file.txt")); !os.IsNotExist(err) {
		t.Error("file.txt should not be extracted when subdir is set")
	}
}

func TestTar_RoundTrip(t *testing.T) {
	cases := []struct {
		name        string
		compression string
		level       int
	}{
		{"uncompressed", "none", 0},
		{"default", "", 0},
		{"gzip", "gzip", 0},
		{"gzip with level", "gzip", 5},
		{"zstd", "zstd", 0},
		{"lz4", "lz4", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
			os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)
			os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

			arc := &Archive{ID: "a", In: "src", Out: "out.tar", Format: "tar", Compression: tc.compression, Level: tc.level}
			if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
				t.Fatalf("archive Apply() error = %v", r.Error)
			}
			un := &Unarchive{ID: "u", In: "out.tar", Out: "extracted"}
			if r := un.Apply(t.Context(), tmpDir); r.Error != nil {
				t.Fatalf("unarchive Apply() error = %v", r.Error)
			}

			got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "sub", "nested.txt"))
			if err != nil || string(got) != "nested" {
				t.Fatalf("sub/nested.txt = %q, err = %v", got, err)
			}
		})
	}
}

func TestUnarchive_UnrecognizedFormat(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "junk.bin"), []byte("not an archive at all"), 0o644)

	un := &Unarchive{ID: "u", In: "junk.bin", Out: "extracted"}
	if r := un.Apply(t.Context(), tmpDir); r.Error == nil {
		t.Error("expected error for unrecognized archive format")
	}
}
