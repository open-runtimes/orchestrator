package erofs

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"orchestrator/internal/erofs/disk"
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
		{"lz4hc split profiles", CompressionOptions{
			Algorithm:           "lz4hc",
			PClusterSize:        128 << 10,
			MaxExtentSize:       4 << 20,
			Fragments:           true,
			PackedPClusterSize:  32 << 10,
			PackedMaxExtentSize: 256 << 10,
		}},
		{"lz4hc dedupe", CompressionOptions{Algorithm: "lz4hc", Dedupe: true}},
		{"lz4hc fragments dedupe", CompressionOptions{Algorithm: "lz4hc", Fragments: true, Dedupe: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertImageMatches(t, buildImage(t, src, WithCompression(tc.opts)), src)
		})
	}
}

func TestCompressedUsesCompactIndexes(t *testing.T) {
	src := fstest.MapFS{
		// A one-lcluster non-fragment exercises the short-array alignment rule.
		"one-cluster":   {Data: bytes.Repeat([]byte("compact "), 300)},
		"many-clusters": {Data: bytes.Repeat([]byte("compact indexes "), 12000)},
	}
	imgPath := buildImage(t, src, WithCompression(CompressionOptions{Algorithm: "lz4hc"}))
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	imgFS, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	img := imgFS.(*image)
	for name := range src {
		opened, err := img.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := opened.(*file).readInfo()
		if err != nil {
			t.Fatal(err)
		}
		if fi.inodeLayout != disk.LayoutCompressedCompact {
			t.Fatalf("%s layout = %d, want compact", name, fi.inodeLayout)
		}
	}
	assertImageMatches(t, imgPath, src)
}

func TestCompactIndexEligibility(t *testing.T) {
	entry := &erofsEntry{z: &zInfo{extents: []zExtent{
		{blkOff: 10, blocks: 2},
		{blkOff: 12, blocks: 1},
	}}}
	if !(&erofsWriter{blockSize: 4096}).zCanCompact(entry) {
		t.Fatal("contiguous 4 KiB extents should use compact indexes")
	}
	entry.z.extents[1].blkOff = 20
	if (&erofsWriter{blockSize: 4096}).zCanCompact(entry) {
		t.Fatal("non-contiguous deduplicated extents need full indexes")
	}
	entry.z.extents[1].blkOff = 12
	if (&erofsWriter{blockSize: 8192}).zCanCompact(entry) {
		t.Fatal("8 KiB blocks need compacted-4B or full indexes")
	}
}

func TestCompressedFullIndexDedupeFallback(t *testing.T) {
	blockA := bytes.Repeat([]byte{'a'}, 4096)
	blockB := bytes.Repeat([]byte{'b'}, 4096)
	src := fstest.MapFS{
		"dedupe-gap": {Data: append(append(append([]byte{}, blockA...), blockB...), blockA...)},
	}
	imgPath := buildImage(t, src, WithCompression(CompressionOptions{
		Algorithm:     "lz4hc",
		PClusterSize:  4096,
		MaxExtentSize: 4096,
		Dedupe:        true,
	}))
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	imgFS, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := imgFS.Open("dedupe-gap")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := opened.(*file).readInfo()
	if err != nil {
		t.Fatal(err)
	}
	if fi.inodeLayout != disk.LayoutCompressedFull {
		t.Fatalf("deduplicated non-contiguous layout = %d, want full", fi.inodeLayout)
	}
	assertImageMatches(t, imgPath, src)
}

func TestCompressionStats(t *testing.T) {
	src := compressTestTree()
	out, err := os.Create(filepath.Join(t.TempDir(), "stats.img"))
	if err != nil {
		t.Fatal(err)
	}
	w := Create(out, WithCompactInodes(), WithCompression(CompressionOptions{
		Algorithm: "lz4hc",
		Fragments: true,
		Dedupe:    true,
	}))
	if err := w.CopyFrom(src); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	stats := w.CompressionStats()
	if stats.InputFiles == 0 || stats.InputBytes == 0 || stats.StoredExtents == 0 {
		t.Fatalf("missing basic compression accounting: %+v", stats)
	}
	if stats.PhysicalBytes != stats.EncodedBytes+stats.RawBytes+stats.PaddingBytes {
		t.Fatalf("physical bytes = %d, encoded + raw + padding = %d",
			stats.PhysicalBytes, stats.EncodedBytes+stats.RawBytes+stats.PaddingBytes)
	}
	if stats.PackedLogicalBytes == 0 || stats.PackedPhysicalBytes == 0 {
		t.Fatalf("missing packed-inode accounting: %+v", stats)
	}
	if stats.FragmentDedupeFiles == 0 || stats.FragmentDedupeBytes == 0 {
		t.Fatalf("expected duplicate fragments in test corpus: %+v", stats)
	}
	if stats.CompressedIndexBytes == 0 || stats.MetadataBytes == 0 {
		t.Fatalf("missing metadata accounting: %+v", stats)
	}
}

func TestFragmentOrder(t *testing.T) {
	src := fstest.MapFS{
		"a": {Data: []byte("aaaa")},
		"b": {Data: []byte("bbbbbb")},
		"c": {Data: []byte("cc")},
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "ordered.img"))
	if err != nil {
		t.Fatal(err)
	}
	w := Create(out, WithCompression(CompressionOptions{
		Algorithm:     "lz4hc",
		Fragments:     true,
		FragmentOrder: []string{"c", "/a", "c"},
	}))
	if err := w.CopyFrom(src); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	offsets := make(map[string]uint64)
	for _, e := range collectRegular(w.root) {
		if e.z == nil || !e.z.wholeFragment {
			t.Fatalf("%s was not packed", e.path)
		}
		offsets[e.path] = e.z.fragOff
	}
	if offsets["/c"] != 0 || offsets["/a"] != 2 || offsets["/b"] != 6 {
		t.Fatalf("fragment offsets = %#v, want c=0 a=2 b=6", offsets)
	}
	assertImageMatches(t, out.Name(), src)
}

func TestWholeFileDedupeBeforeCompression(t *testing.T) {
	duplicate := bytes.Repeat([]byte("large shared input "), 40_000)
	src := fstest.MapFS{
		"a":      {Data: duplicate},
		"b":      {Data: bytes.Clone(duplicate)},
		"unique": {Data: bytes.Repeat([]byte("different input "), 40_000)},
	}
	out, err := os.Create(filepath.Join(t.TempDir(), "whole-file-dedupe.img"))
	if err != nil {
		t.Fatal(err)
	}
	w := Create(out, WithCompression(CompressionOptions{Algorithm: "lz4", Dedupe: true}))
	if err := w.CopyFrom(src); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	entries := make(map[string]*fsEntry)
	for _, e := range collectRegular(w.root) {
		entries[e.path] = e
	}
	if entries["/a"].z == nil || entries["/a"].z != entries["/b"].z {
		t.Fatal("large duplicate did not reuse the canonical compressed layout")
	}
	if entries["/unique"].z == entries["/a"].z {
		t.Fatal("different file reused the duplicate layout")
	}
	assertImageMatches(t, out.Name(), src)
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
		"unaligned extent":      {Algorithm: "lz4", MaxExtentSize: 5000},
		"oversized extent":      {Algorithm: "lz4", MaxExtentSize: 13 << 20},
		"unaligned packed pcluster": {
			Algorithm: "lz4", PackedPClusterSize: 5000,
		},
		"oversized packed pcluster": {
			Algorithm: "lz4", PackedPClusterSize: 2 << 20,
		},
		"unaligned packed extent": {
			Algorithm: "lz4", PackedMaxExtentSize: 5000,
		},
		"oversized packed extent": {
			Algorithm: "lz4", PackedMaxExtentSize: 13 << 20,
		},
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

// TestCompressedLargeFile crosses many pclusters, the 16-bit NONHEAD delta
// encoding, and the segment-parallel packing path (the file spans multiple
// 8 MiB segments).
func TestCompressedLargeFile(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 20<<20; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%97))
		b.WriteString("\n")
	}
	// The fast encoder keeps this affordable on small CI runners; the
	// parallel machinery under test is identical for both algorithms, and
	// lz4hc's encoders have their own corpora in lz4enc_test.go.
	src := fstest.MapFS{"large.txt": {Data: []byte(b.String())}}
	img := buildImage(t, src, WithCompression(CompressionOptions{Algorithm: "lz4", PClusterSize: 65536}))
	assertImageMatches(t, img, src)
}

// TestCompressedMaximumExtent exercises a pcluster spanning more than 2048
// logical blocks. NONHEAD indexes beyond that point must use bounded backward
// hops because bit 11 is reserved for the compressed-block-count marker.
func TestCompressedMaximumExtent(t *testing.T) {
	src := fstest.MapFS{
		"ten-megabytes": {Data: bytes.Repeat([]byte("highly compressible module body\n"), 350000)},
	}
	img := buildImage(t, src, WithCompression(CompressionOptions{
		Algorithm:     "lz4",
		PClusterSize:  1 << 20,
		MaxExtentSize: 12 << 20,
	}))
	assertImageMatches(t, img, src)
}

// TestPackedProfileBoundsDecodedExtents pins the random-read amplification
// contract of the independent packed-inode profile.
func TestPackedProfileBoundsDecodedExtents(t *testing.T) {
	src := fstest.MapFS{
		"a": {Data: bytes.Repeat([]byte("small module a\n"), 20000)},
		"b": {Data: bytes.Repeat([]byte("small module b\n"), 20000)},
	}
	const maxDecoded = 64 << 10
	path := buildImage(t, src, WithCompression(CompressionOptions{
		Algorithm:           "lz4",
		PClusterSize:        128 << 10,
		MaxExtentSize:       4 << 20,
		Fragments:           true,
		PackedPClusterSize:  32 << 10,
		PackedMaxExtentSize: maxDecoded,
	}))
	assertImageMatches(t, path, src)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	img := opened.(*image)
	packed, err := img.zPackedInfo()
	if err != nil {
		t.Fatal(err)
	}
	for lcn := 0; lcn < packed.z.nIdx; {
		advise, _, blkaddr, err := img.zIndex(packed, lcn)
		if err != nil {
			t.Fatal(err)
		}
		switch advise & 0x3 {
		case zLclusterPlain:
			lcn++
		case zLclusterHead1:
			data, err := img.zExtentData(packed, lcn, blkaddr)
			if err != nil {
				t.Fatal(err)
			}
			if len(data) > maxDecoded {
				t.Fatalf("packed extent decodes to %d bytes, limit %d", len(data), maxDecoded)
			}
			blockSize := int(img.blockSize())
			lcn += (len(data) + blockSize - 1) / blockSize
		default:
			t.Fatalf("walk landed on NONHEAD index at lcn %d", lcn)
		}
	}
}

// TestCompressedMetadataFirst keeps inode and directory metadata ahead of
// the compressed payload, avoiding long seeks during cold path lookup.
func TestCompressedMetadataFirst(t *testing.T) {
	path := buildImage(t, compressTestTree(), WithCompression(CompressionOptions{Algorithm: "lz4"}))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opened, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.(*image).sb.MetaBlkAddr; got != 1 {
		t.Fatalf("metadata starts at block %d, want block 1", got)
	}
}

type countingReaderAt struct {
	io.ReaderAt
	reads int
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	return r.ReaderAt.ReadAt(p, off)
}

// TestPackedExtentCacheSharedAcrossReads verifies that fragment reads reuse
// both the parsed packed inode and its decompressed extent.
func TestPackedExtentCacheSharedAcrossReads(t *testing.T) {
	src := fstest.MapFS{
		"a": {Data: bytes.Repeat([]byte("cached fragment a\n"), 200)},
		"b": {Data: bytes.Repeat([]byte("cached fragment b\n"), 200)},
	}
	path := buildImage(t, src, WithCompression(CompressionOptions{
		Algorithm: "lz4", Fragments: true,
	}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingReaderAt{ReaderAt: bytes.NewReader(raw)}
	opened, err := Open(counter)
	if err != nil {
		t.Fatal(err)
	}
	img := opened.(*image)
	f, err := img.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := f.(*file).readInfo()
	if err != nil {
		t.Fatal(err)
	}
	if err := img.zInit(fi); err != nil {
		t.Fatal(err)
	}
	if !fi.z.wholeFragment {
		t.Fatal("test file was not stored as a whole fragment")
	}

	want := src["a"].Data[:128]
	got := make([]byte, len(want))
	if err := img.zReadPacked(fi.z.fragOff, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("first packed read returned the wrong data")
	}
	reads := counter.reads
	clear(got)
	if err := img.zReadPacked(fi.z.fragOff, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("cached packed read returned the wrong data")
	}
	if counter.reads != reads {
		t.Fatalf("cached packed read issued %d additional ReaderAt calls", counter.reads-reads)
	}
}

// TestCompressedRejectsHostilePClusterSizes regresses the reader allocating
// attacker-controlled amounts of memory: the compressed block count and the
// decompressed extent size both come from on-disk indexes, and a crafted
// image must be rejected before allocation, not trusted.
func TestCompressedRejectsHostilePClusterSizes(t *testing.T) {
	src := fstest.MapFS{"big.txt": {Data: bytes.Repeat([]byte("compressible line\n"), 20000)}}
	path := buildImage(t, src, WithCompression(CompressionOptions{Algorithm: "lz4"}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Locate the file's first lcluster index the same way the reader does.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := img.Open("big.txt")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := opened.(*file).readInfo()
	if err != nil {
		t.Fatal(err)
	}
	i := img.(*image)
	if err := i.zInit(fi); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// The first NONHEAD (index 1) carries CBLKCNT|blocks in delta[0]; claim
	// the pcluster spans 2047 blocks (8 MiB at 4 KiB blocks — over the 1 MiB
	// format limit).
	hostile := append([]byte(nil), raw...)
	if fi.z.compact {
		initial := ((32 - int(fi.z.idxStart%32)) / 4) & 7
		middle := 0
		if initial < fi.z.nIdx {
			middle = (fi.z.nIdx - initial) &^ 15
		}
		lcn := 1
		var packStart int64
		var idx, vcnt, destSize int
		switch {
		case lcn < initial:
			vcnt, destSize, idx, packStart = 2, 4, lcn, fi.z.idxStart
		case lcn < initial+middle:
			vcnt, destSize, idx = 16, 2, lcn-initial
			packStart = fi.z.idxStart + int64(initial*4)
		default:
			vcnt, destSize, idx = 2, 4, lcn-initial-middle
			packStart = fi.z.idxStart + int64(initial*4+middle*2)
		}
		packBytes := vcnt * destSize
		packStart += int64(idx/vcnt) * int64(packBytes)
		encodeBits := (packBytes*8 - 32) / vcnt
		bitpos := encodeBits * (idx % vcnt)
		value := uint32(zLclusterNonhead<<12 | zCblkcntBit | 0x7ff)
		for bit := range encodeBits {
			pos := int(packStart)*8 + bitpos + bit
			hostile[pos/8] &^= 1 << (pos & 7)
			if value&(1<<bit) != 0 {
				hostile[pos/8] |= 1 << (pos & 7)
			}
		}
	} else {
		firstNonhead := fi.z.idxStart + zLclusterIdxSize
		binary.LittleEndian.PutUint16(hostile[firstNonhead+4:], zCblkcntBit|0x7ff)
	}

	img2, err := Open(bytes.NewReader(hostile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(img2, "big.txt"); err == nil || !strings.Contains(err.Error(), "format limit") {
		t.Fatalf("hostile block count: got err %v, want format-limit rejection", err)
	}
}

func TestCompressedResultBufferPools(t *testing.T) {
	packed := zProfile{pclusterSize: 4096, maxExtentSize: 16384}
	data := zProfile{pclusterSize: 8192, maxExtentSize: 32768}
	z := &zState{
		packedProfile:  packed,
		dataProfile:    data,
		packedCompBufs: make(chan []byte, 1),
		dataCompBufs:   make(chan []byte, 1),
	}

	scratch := []byte("completed compressed block")
	staged := z.stageCompressed(packed, scratch)
	if !bytes.Equal(staged, scratch) {
		t.Fatalf("staged bytes = %q, want %q", staged, scratch)
	}
	if cap(staged) != packed.pclusterSize {
		t.Fatalf("packed staged capacity = %d, want %d", cap(staged), packed.pclusterSize)
	}
	// The result must not alias compressor scratch while it waits for ordered
	// collection.
	scratch[0] ^= 0xff
	if bytes.Equal(staged, scratch) {
		t.Fatal("staged result aliases compressor scratch")
	}

	first := &staged[:cap(staged)][0]
	z.releaseCompressed(packed, staged)
	reused := z.stageCompressed(packed, []byte("next block"))
	if &reused[:cap(reused)][0] != first {
		t.Fatal("packed result buffer was not reused")
	}

	dataResult := z.stageCompressed(data, []byte("data profile block"))
	if cap(dataResult) != data.pclusterSize {
		t.Fatalf("data staged capacity = %d, want %d", cap(dataResult), data.pclusterSize)
	}
	spans := []packedSpan{{comp: reused}, {comp: nil}, {comp: dataResult}}
	// Release each span through its owning profile, as the real collectors do.
	z.releasePackedSpans(packed, spans[:2])
	z.releasePackedSpans(data, spans[2:])
	for i := range spans {
		if spans[i].comp != nil {
			t.Fatalf("span %d retained released result buffer", i)
		}
	}
	if len(z.packedCompBufs) != 1 || len(z.dataCompBufs) != 1 {
		t.Fatalf("pooled buffers: packed=%d data=%d, want one each", len(z.packedCompBufs), len(z.dataCompBufs))
	}
}

func TestStreamPackerCloseReleasesQueuedResults(t *testing.T) {
	profile := zProfile{pclusterSize: 4096, maxExtentSize: 16384}
	z := &zState{
		packedProfile:  profile,
		packedCompBufs: make(chan []byte, 2),
		segmentBufs:    make(chan []byte, 2),
	}
	comp := z.stageCompressed(profile, []byte("queued compressed block"))
	segment := make([]byte, 128, zSegmentSize)
	result := make(chan zStreamResult, 1)
	result <- zStreamResult{
		packedSegment: packedSegment{spans: []packedSpan{{comp: comp}}},
		buf:           segment,
	}
	p := &zStreamPacker{
		z:       z,
		profile: profile,
		queue:   []chan zStreamResult{result},
		tail:    make([]byte, 64, zSegmentSize),
		closed:  true, // no worker goroutines in this focused ownership test
	}

	p.Close()
	if len(p.queue) != 0 || p.tail != nil {
		t.Fatalf("close retained queue=%d tail=%v", len(p.queue), p.tail != nil)
	}
	if len(z.packedCompBufs) != 1 {
		t.Fatalf("pooled compressed buffers = %d, want 1", len(z.packedCompBufs))
	}
	if len(z.segmentBufs) != 2 {
		t.Fatalf("pooled segment buffers = %d, want 2", len(z.segmentBufs))
	}
}
