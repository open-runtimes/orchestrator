package artifact

import (
	"bytes"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"orchestrator/internal/erofs"
)

// assertErofsContents opens the image (test-only; production mounts, never
// extracts) and verifies the expected files round-tripped.
func assertErofsContents(t *testing.T, image string) {
	t.Helper()
	f, err := os.Open(image)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()
	img, err := erofs.Open(f)
	if err != nil {
		t.Fatalf("erofs.Open() error = %v", err)
	}

	if got, err := fs.ReadFile(img, "file.txt"); err != nil || string(got) != "hello" {
		t.Fatalf("file.txt = %q, err = %v", got, err)
	}
	if got, err := fs.ReadFile(img, "sub/nested.txt"); err != nil || string(got) != "nested" {
		t.Fatalf("sub/nested.txt = %q, err = %v", got, err)
	}
}

func TestErofsCompressionProfile(t *testing.T) {
	opts, compressed, err := erofsCompression("lz4hc")
	if err != nil || !compressed {
		t.Fatalf("erofsCompression(lz4hc) = (%+v, %v, %v)", opts, compressed, err)
	}
	if opts.PClusterSize != 128<<10 || opts.MaxExtentSize != 4<<20 ||
		opts.PackedPClusterSize != 64<<10 || opts.PackedMaxExtentSize != 1<<20 ||
		!opts.Fragments || !opts.Dedupe {
		t.Fatalf("unexpected balanced erofs profile: %+v", opts)
	}
}

func TestArchive_Erofs_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "out.erofs", Format: "erofs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	image := filepath.Join(tmpDir, "out.erofs")
	magic, err := os.ReadFile(image)
	if err != nil || !isErofs(magic) {
		t.Fatalf("output is not an erofs image (err=%v)", err)
	}
	assertErofsContents(t, image)
}

// TestArchive_Erofs_Compressed round-trips lz4 and lz4hc images (which enable
// fragments and dedupe) through archive and unarchive, with content spanning
// the whole-file-fragment, multi-extent, and incompressible paths.
func TestArchive_Erofs_Compressed(t *testing.T) {
	for _, comp := range []string{"lz4", "lz4hc"} {
		t.Run(comp, func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
			big := bytes.Repeat([]byte("compressible content line\n"), 20000)
			random := make([]byte, 300000)
			rand.New(rand.NewSource(1)).Read(random)
			os.WriteFile(filepath.Join(srcDir, "small.txt"), []byte("hello"), 0o644)
			os.WriteFile(filepath.Join(srcDir, "small2.txt"), []byte("hello"), 0o644) // dedupes with small.txt
			os.WriteFile(filepath.Join(srcDir, "big.txt"), big, 0o644)
			os.WriteFile(filepath.Join(srcDir, "sub", "random.bin"), random, 0o644)

			arc := &Archive{ID: "a", In: "src", Out: "out.erofs", Format: "erofs", Compression: comp}
			if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
				t.Fatalf("archive Apply() error = %v", r.Error)
			}
			image := filepath.Join(tmpDir, "out.erofs")
			magic, err := os.ReadFile(image)
			if err != nil || !isErofs(magic) {
				t.Fatalf("output is not an erofs image (err=%v)", err)
			}
			if len(magic) >= len(big) {
				t.Fatalf("image (%d bytes) is not smaller than its most compressible input (%d bytes)", len(magic), len(big))
			}

			un := &Unarchive{ID: "u", In: "out.erofs", Out: "extracted"}
			if r := un.Apply(t.Context(), tmpDir); r.Error != nil {
				t.Fatalf("unarchive Apply() error = %v", r.Error)
			}
			for name, want := range map[string][]byte{
				"small.txt":      []byte("hello"),
				"small2.txt":     []byte("hello"),
				"big.txt":        big,
				"sub/random.bin": random,
			} {
				got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("%s: mismatch (err=%v, %d vs %d bytes)", name, err, len(got), len(want))
				}
			}
		})
	}
}

// TestArchive_Erofs_SingleFile covers the single-file path (streamed via a
// one-entry fs.FS, not buffered into memory): the file lands at the image root.
func TestArchive_Erofs_SingleFile(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "solo.txt"), []byte("solo content"), 0o644)

	arc := &Archive{ID: "a", In: "solo.txt", Out: "out.erofs", Format: "erofs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	f, err := os.Open(filepath.Join(tmpDir, "out.erofs"))
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer f.Close()
	img, err := erofs.Open(f)
	if err != nil {
		t.Fatalf("erofs.Open() error = %v", err)
	}
	if got, err := fs.ReadFile(img, "solo.txt"); err != nil || string(got) != "solo content" {
		t.Fatalf("solo.txt = %q, err = %v", got, err)
	}
}

// TestUnarchive_ExtractsErofs round-trips an erofs image back out through
// unarchive (the materializing path, vs. the mount artifact).
func TestUnarchive_ExtractsErofs(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.erofs", Format: "erofs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.erofs", Out: "extracted"}
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

// TestUnarchive_ExtractsErofs_Strip drops the single wrapper directory.
func TestUnarchive_ExtractsErofs_Strip(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "repo", "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "repo", "file.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "repo", "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.erofs", Format: "erofs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.erofs", Out: "extracted", Strip: true}
	if r := un.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("unarchive Apply() error = %v", r.Error)
	}

	if got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "file.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("file.txt = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "extracted", "repo")); !os.IsNotExist(err) {
		t.Fatal("wrapper root directory should not exist in extracted directory")
	}
}

// TestUnarchive_ExtractsErofs_Subdir extracts only a subtree.
func TestUnarchive_ExtractsErofs_Subdir(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "src")
	os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "sub", "nested.txt"), []byte("nested"), 0o644)

	arc := &Archive{ID: "a", In: "src", Out: "data.erofs", Format: "erofs"}
	if r := arc.Apply(t.Context(), tmpDir); r.Error != nil {
		t.Fatalf("archive Apply() error = %v", r.Error)
	}

	un := &Unarchive{ID: "u", In: "data.erofs", Out: "extracted", Subdir: "sub"}
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
