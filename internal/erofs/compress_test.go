package erofs

import (
	"bytes"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// compressTestTree exercises every write path: empty and tiny files (flat),
// whole-file fragments, single- and multi-extent compressed files, an EOF
// boundary marker (size not block-aligned), incompressible PLAIN extents,
// a mixed file, exact block-multiple sizes, duplicates for dedupe, and a
// symlink plus nested directories for the untouched paths.
func compressTestTree() fstest.MapFS {
	rnd := rand.New(rand.NewSource(7))
	random := make([]byte, 200000)
	rnd.Read(random)
	mixed := append(bytes.Repeat([]byte{'A'}, 100000), random[:100000]...)

	return fstest.MapFS{
		"empty":            {Data: nil},
		"tiny":             {Data: []byte("x")},
		"exact4k":          {Data: bytes.Repeat([]byte{'a'}, 4096)},
		"exact128k":        {Data: bytes.Repeat([]byte("bc"), 65536)},
		"big.txt":          {Data: bytes.Repeat([]byte("The quick brown fox. "), 8000)},
		"random.bin":       {Data: random},
		"sub/mixed":        {Data: mixed},
		"sub/deep/dup1":    {Data: bytes.Repeat([]byte("shared content "), 3000)},
		"sub/deep/dup2":    {Data: bytes.Repeat([]byte("shared content "), 3000)},
		"sub/deep/smalldu": {Data: []byte("small duplicate body")},
		"sub/smalldup2":    {Data: []byte("small duplicate body")},
		"link":             {Data: []byte("sub/mixed"), Mode: fs.ModeSymlink},
	}
}

// buildImage writes src into a fresh image file and returns its path.
func buildImage(t *testing.T, src fs.FS, opts ...CreateOpt) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.img")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := Create(out, opts...)
	if err := w.CopyFrom(src); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertImageMatches opens an image and byte-compares every regular file and
// symlink against the source tree.
func assertImageMatches(t *testing.T, imgPath string, src fstest.MapFS) {
	t.Helper()
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := Open(f)
	if err != nil {
		t.Fatalf("Open image: %v", err)
	}
	for name, want := range src {
		if want.Mode&fs.ModeSymlink != 0 {
			got, err := img.(fs.ReadLinkFS).ReadLink(name)
			if err != nil || got != string(want.Data) {
				t.Fatalf("readlink %s = %q, err %v; want %q", name, got, err, want.Data)
			}
			continue
		}
		got, err := fs.ReadFile(img, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(got, want.Data) {
			t.Fatalf("%s: content mismatch (%d vs %d bytes)", name, len(got), len(want.Data))
		}
	}
}

func TestCompressedRoundTrip(t *testing.T) {
	src := compressTestTree()
	for _, tc := range []struct {
		name string
		opts CompressionOptions
	}{
		{"lz4", CompressionOptions{Algorithm: "lz4"}},
		{"lz4hc", CompressionOptions{Algorithm: "lz4hc"}},
		{"lz4hc big pcluster", CompressionOptions{Algorithm: "lz4hc", PClusterSize: 131072}},
		{"lz4hc fragments", CompressionOptions{Algorithm: "lz4hc", Fragments: true}},
		{"lz4hc dedupe", CompressionOptions{Algorithm: "lz4hc", Dedupe: true}},
		{"lz4hc fragments dedupe", CompressionOptions{Algorithm: "lz4hc", Fragments: true, Dedupe: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertImageMatches(t, buildImage(t, src, WithCompression(tc.opts)), src)
		})
	}
}

// TestCompressedShrinks pins that compression actually reduces the image and
// that dedupe removes the duplicated file's blocks.
func TestCompressedShrinks(t *testing.T) {
	src := compressTestTree()
	size := func(opts ...CreateOpt) int64 {
		st, err := os.Stat(buildImage(t, src, opts...))
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	plain := size()
	comp := size(WithCompression(CompressionOptions{Algorithm: "lz4hc"}))
	deduped := size(WithCompression(CompressionOptions{Algorithm: "lz4hc", Fragments: true, Dedupe: true}))
	if comp >= plain {
		t.Fatalf("compressed image (%d) not smaller than uncompressed (%d)", comp, plain)
	}
	if deduped >= comp {
		t.Fatalf("deduped+fragments image (%d) not smaller than compressed (%d)", deduped, comp)
	}
}

// TestCompressedRejectsInvalidOptions covers option validation.
func TestCompressedRejectsInvalidOptions(t *testing.T) {
	for name, opts := range map[string]CompressionOptions{
		"bad algorithm":         {Algorithm: "zstd"},
		"unaligned pcluster":    {Algorithm: "lz4", PClusterSize: 5000},
		"oversized pcluster":    {Algorithm: "lz4", PClusterSize: 2 << 20},
		"negative-ish pcluster": {Algorithm: "lz4", PClusterSize: -4096},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := os.Create(filepath.Join(t.TempDir(), "img"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			w := Create(out, WithCompression(opts))
			if err := w.CopyFrom(fstest.MapFS{"f": {Data: []byte("data")}}); err != nil {
				t.Fatalf("CopyFrom: %v", err)
			}
			if err := w.Close(); err == nil {
				t.Fatal("Close succeeded with invalid compression options")
			}
		})
	}
}

// TestCompressedLargeFile crosses many pclusters and the 16-bit NONHEAD
// delta encoding paths with a file larger than 100 pclusters.
func TestCompressedLargeFile(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 7<<20; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%97))
		b.WriteString("\n")
	}
	src := fstest.MapFS{"large.txt": {Data: []byte(b.String())}}
	img := buildImage(t, src, WithCompression(CompressionOptions{Algorithm: "lz4hc", PClusterSize: 65536}))
	assertImageMatches(t, img, src)
}
