package erofs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"github.com/pierrec/lz4/v4"
	"orchestrator/internal/erofs/disk"
)

// CompressionOptions configures z_erofs compressed output. Compressed images
// use the COMPRESSED_FULL (non-compact index) inode layout with 0-padded lz4
// pclusters, matching what mkfs.erofs -E legacy-compress emits and what every
// kernel since 5.4 mounts.
type CompressionOptions struct {
	// Algorithm selects the block compressor: "lz4" (fast encoder) or
	// "lz4hc" (high-compression encoder, identical decompression).
	Algorithm string
	// PClusterSize is the maximum uncompressed byte span encoded into one
	// physical cluster. Must be a multiple of the block size, at most 1 MiB.
	// 0 defaults to 65536.
	PClusterSize int
	// Fragments packs each regular file smaller than PClusterSize whole into
	// the shared packed inode, so many small files compress together instead
	// of each paying block-alignment padding.
	Fragments bool
	// Dedupe reuses identical pclusters and fragments across files, keyed by
	// content hash.
	Dedupe bool
}

// zExtent describes one physical cluster of a compressed file: `lclusters`
// logical clusters decoded from `blocks` physical blocks at `blkOff` within
// the compressed data region. Plain extents store data raw, one block per
// lcluster.
type zExtent struct {
	lclusters int
	blocks    int
	blkOff    uint32
	plain     bool
}

// zInfo is the compressed-layout state of one regular file.
type zInfo struct {
	extents     []zExtent
	totalBlocks uint32
	// wholeFragment means the entire file lives in the packed inode at
	// fragOff and the inode carries no lcluster indexes at all.
	wholeFragment bool
	fragOff       uint64
}

// zState carries compression results from the compression pass to the
// image writer.
type zState struct {
	opts       CompressionOptions
	compSpool  *os.File // block-aligned pcluster images, in blkOff order
	compBlocks uint32   // total blocks in the compressed data region
	compBase   uint32   // first block of the compressed region (set at layout)
	packed     *erofsEntry
	// dedupe maps chunk content hash to its stored extent shape.
	dedupe map[[sha256.Size]byte]zExtent
	// fragDedupe maps whole-file content hash to a packed inode offset.
	fragDedupe map[[sha256.Size]byte]uint64
	packedSize uint64
	packedTmp  *os.File // raw fragment bytes until the packed inode is built
	blockSize  int
	// newCompressor returns a block compressor; instances are not safe for
	// concurrent use, so each packing goroutine takes its own.
	newCompressor func() func(src, dst []byte) (int, error)
	// newFinalizer, when set, returns a slower, stronger compressor used to
	// re-encode each chosen span once (span sizing probes use newCompressor).
	newFinalizer func() func(src, dst []byte) (int, error)
	workers      int
}

// zSelfCheck decompresses every stored span and compares it against the
// input before it reaches the image — a debugging aid for encoder work,
// too expensive to leave on.
const zSelfCheck = false

const (
	zDefaultPClusterSize = 65536
	// lcluster index di_advise types.
	zLclusterPlain   = 0
	zLclusterHead1   = 1
	zLclusterNonhead = 2
	// delta[0] flag on the first NONHEAD carrying the compressed block count.
	zCblkcntBit = 1 << 11
	// h_advise: physical clusters may span more than one block.
	zAdviseBigPcluster1 = 0x0002
	// map header size plus the 8 reserved bytes before full indexes.
	zMapHeaderSize   = 8
	zFullIndexExtra  = 8
	zLclusterIdxSize = 8
)

// WithCompression enables z_erofs compressed output for regular file data.
func WithCompression(c CompressionOptions) CreateOpt {
	return func(o *createOptions) {
		o.compression = &c
	}
}

// WithCompactInodes stores every eligible inode in the 32-byte compact form
// even when that drops its mtime (readers then report the image build time,
// exactly like mkfs.erofs does by default). Roughly halves per-inode metadata
// on trees with many files.
func WithCompactInodes() CreateOpt {
	return func(o *createOptions) {
		o.compactInodes = true
	}
}

func newZState(opts CompressionOptions, blockSize int, tempDir string) (*zState, error) {
	if opts.PClusterSize == 0 {
		opts.PClusterSize = zDefaultPClusterSize
	}
	if opts.PClusterSize%blockSize != 0 || opts.PClusterSize <= 0 || opts.PClusterSize > 1<<20 {
		return nil, fmt.Errorf("erofs: pcluster size %d must be a positive multiple of block size %d, at most 1 MiB", opts.PClusterSize, blockSize)
	}
	z := &zState{
		opts:      opts,
		blockSize: blockSize,
	}
	switch opts.Algorithm {
	case "", "lz4":
		z.newCompressor = func() func(src, dst []byte) (int, error) {
			var c lz4.Compressor
			return c.CompressBlock
		}
	case "lz4hc":
		z.newCompressor = func() func(src, dst []byte) (int, error) {
			var c lz4HCEncoder
			return c.CompressBlock
		}
		z.newFinalizer = func() func(src, dst []byte) (int, error) {
			var c lz4HCEncoder
			return c.CompressBlockOpt
		}
	default:
		return nil, fmt.Errorf("erofs: unsupported compression algorithm %q (supported: lz4, lz4hc)", opts.Algorithm)
	}
	z.workers = min(runtime.GOMAXPROCS(0), 8)
	if opts.Dedupe {
		z.dedupe = make(map[[sha256.Size]byte]zExtent)
		z.fragDedupe = make(map[[sha256.Size]byte]uint64)
	}
	var err error
	if z.compSpool, err = os.CreateTemp(tempDir, "erofs-comp-*"); err != nil {
		return nil, err
	}
	os.Remove(z.compSpool.Name())
	if opts.Fragments {
		if z.packedTmp, err = os.CreateTemp(tempDir, "erofs-packed-*"); err != nil {
			z.compSpool.Close()
			return nil, err
		}
		os.Remove(z.packedTmp.Name())
	}
	return z, nil
}

func (z *zState) close() {
	if z.compSpool != nil {
		z.compSpool.Close()
	}
	if z.packedTmp != nil {
		z.packedTmp.Close()
	}
}

// compressAll runs the compression pass over every eligible regular file,
// replacing its flat data with compressed extents (or a packed-inode
// fragment), then builds the packed inode itself. Small files pack whole
// into the packed inode inline; mid-size files fan out to a worker pool
// (collected in tree order, so output stays deterministic); large files
// stream through the segment-parallel packer one at a time.
func (fsys *Writer) compressAll() error {
	z := fsys.z

	type fileResult struct {
		spans []packedSpan
		err   error
	}
	type fileJob struct {
		e   *fsEntry
		buf []byte
		res chan fileResult
	}
	jobs := make(chan *fileJob)
	var wg sync.WaitGroup
	for w := 0; w < z.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			compress := z.newCompressor()
			var finalize func(src, dst []byte) (int, error)
			if z.newFinalizer != nil {
				finalize = z.newFinalizer()
			}
			probe := make([]byte, lz4.CompressBlockBound(zMaxSpan))
			kept := make([]byte, lz4.CompressBlockBound(zMaxSpan))
			for job := range jobs {
				spans, err := z.packBuffer(job.buf, compress, finalize, &probe, &kept)
				job.res <- fileResult{spans: spans, err: err}
			}
		}()
	}
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	// collect resolves one finished job into its entry's extent list.
	pending := make([]*fileJob, 0, z.workers*2)
	collect := func(job *fileJob) error {
		res := <-job.res
		if res.err != nil {
			return fmt.Errorf("erofs: compress %s: %w", job.e.path, res.err)
		}
		zi := &zInfo{}
		for _, ps := range res.spans {
			ext, err := z.storeSpanKeyed(ps.raw, ps.comp, ps.key)
			if err != nil {
				return err
			}
			zi.extents = append(zi.extents, ext)
			zi.totalBlocks += uint32(ext.blocks)
		}
		job.e.z = zi
		return nil
	}
	drain := func() error {
		for _, job := range pending {
			if err := collect(job); err != nil {
				return err
			}
		}
		pending = pending[:0]
		return nil
	}

	for _, e := range collectRegular(fsys.root) {
		var r io.Reader
		switch {
		case e.directData != nil:
			r = e.directData
		case fsys.spool != nil:
			r = io.NewSectionReader(fsys.spool, e.spoolOff, int64(e.size))
		default:
			continue
		}
		switch {
		case z.opts.Fragments && e.size < 4*uint64(z.opts.PClusterSize):
			off, err := z.addFragment(r, int(e.size))
			if err != nil {
				return fmt.Errorf("erofs: pack %s: %w", e.path, err)
			}
			e.z = &zInfo{wholeFragment: true, fragOff: off}
		case e.size >= 2*zSegmentSize:
			// Large file: extents must land in stream order, so drain the
			// pool first; the stream packer parallelizes internally.
			if err := drain(); err != nil {
				return err
			}
			zi, err := z.compressStream(r, e.size)
			if err != nil {
				return fmt.Errorf("erofs: compress %s: %w", e.path, err)
			}
			e.z = zi
		default:
			buf := make([]byte, e.size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return fmt.Errorf("erofs: read %s: %w", e.path, err)
			}
			job := &fileJob{e: e, buf: buf, res: make(chan fileResult, 1)}
			if len(pending) == cap(pending) {
				if err := collect(pending[0]); err != nil {
					return err
				}
				pending = pending[:copy(pending, pending[1:])]
			}
			pending = append(pending, job)
			jobs <- job
		}
		if c, ok := e.directData.(io.Closer); ok {
			_ = c.Close()
		}
		e.directData = nil
	}
	if err := drain(); err != nil {
		return err
	}

	if z.packedTmp != nil && z.packedSize > 0 {
		zi, err := z.compressStream(io.NewSectionReader(z.packedTmp, 0, int64(z.packedSize)), z.packedSize)
		if err != nil {
			return fmt.Errorf("erofs: compress packed inode: %w", err)
		}
		z.packed = &erofsEntry{
			mode:  disk.StatTypeReg | 0o600,
			nlink: 1,
			size:  z.packedSize,
			name:  "packed",
			path:  "(packed)",
			mtime: fsys.buildTime,
			z:     zi,
		}
	}
	return nil
}

// packBuffer packs one whole in-memory stream into spans, hashing them for
// dedupe when enabled. Used by the per-file worker pool.
func (z *zState) packBuffer(buf []byte, compress, finalize func(src, dst []byte) (int, error), probe, kept *[]byte) ([]packedSpan, error) {
	var spans []packedSpan
	window := buf
	ratio := 0.5
	for len(window) > 0 {
		span, comp, err := z.packSpan(compress, window, probe, kept, true, &ratio)
		if err != nil {
			return nil, err
		}
		comp, err = refineSpan(finalize, window[:span], comp, *probe, z.blockSize)
		if err != nil {
			return nil, err
		}
		ps := packedSpan{raw: window[:span], comp: append([]byte(nil), comp...)}
		if z.dedupe != nil {
			ps.key = sha256.Sum256(ps.raw)
		}
		spans = append(spans, ps)
		window = window[span:]
	}
	return spans, nil
}

// collectRegular returns every regular file with data eligible for the
// compressed layout, in deterministic tree order.
func collectRegular(root *fsEntry) []*fsEntry {
	var out []*fsEntry
	var walk func(e *fsEntry)
	walk = func(e *fsEntry) {
		if e.removed {
			return
		}
		if e.mode&disk.StatTypeMask == disk.StatTypeReg &&
			e.size > 0 && len(e.chunks) == 0 && !e.metadataOnly {
			out = append(out, e)
		}
		for _, c := range e.children {
			walk(c)
		}
	}
	walk(root)
	return out
}

// addFragment appends a whole small file to the packed inode data, reusing
// an identical earlier fragment when dedupe is on. Returns the fragment's
// byte offset within the packed inode.
func (z *zState) addFragment(r io.Reader, size int) (uint64, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	var key [sha256.Size]byte
	if z.fragDedupe != nil {
		key = sha256.Sum256(buf)
		if off, ok := z.fragDedupe[key]; ok {
			return off, nil
		}
	}
	off := z.packedSize
	if _, err := z.packedTmp.WriteAt(buf, int64(off)); err != nil {
		return 0, err
	}
	z.packedSize += uint64(size)
	if z.fragDedupe != nil {
		z.fragDedupe[key] = off
	}
	return off, nil
}

// zMaxSpan caps the uncompressed bytes one pcluster may decode to. Larger
// spans improve the ratio on compressible data (more input amortizes each
// pcluster's block padding) but raise the decompression cost of a random
// read; 4 MiB stays far under the kernel's 12 MiB limit and keeps extents
// under the 2048-lcluster bound where NONHEAD deltas would collide with the
// CBLKCNT flag bit.
const zMaxSpan = 4 << 20

// compressStream packs a file into pclusters mkfs.erofs-style: each pcluster
// holds as much input as compresses into PClusterSize bytes (found with a
// ratio-guided search rather than a destsize codec), storing raw blocks when
// compression saves less than one block. Span boundaries are always
// lcluster-aligned, so extents never start mid-lcluster.
func (z *zState) compressStream(r io.Reader, size uint64) (*zInfo, error) {
	if size >= 2*zSegmentSize && z.workers > 1 {
		return z.compressParallel(r, size)
	}
	zi := &zInfo{}
	compress := z.newCompressor()
	var finalize func(src, dst []byte) (int, error)
	if z.newFinalizer != nil {
		finalize = z.newFinalizer()
	}
	window := make([]byte, 0, min(size, zMaxSpan))
	probe := make([]byte, lz4.CompressBlockBound(zMaxSpan))
	kept := make([]byte, lz4.CompressBlockBound(zMaxSpan))
	remaining := size
	ratio := 0.5 // running compressed/uncompressed estimate for this stream
	for remaining > 0 || len(window) > 0 {
		// Refill the window up to zMaxSpan.
		if want := int(min(zMaxSpan, uint64(len(window))+remaining)); len(window) < want {
			off := len(window)
			window = window[:want]
			if _, err := io.ReadFull(r, window[off:]); err != nil {
				return nil, err
			}
			remaining -= uint64(want - off)
		}

		final := remaining == 0
		span, comp, err := z.packSpan(compress, window, &probe, &kept, final, &ratio)
		if err != nil {
			return nil, err
		}
		comp, err = refineSpan(finalize, window[:span], comp, probe, z.blockSize)
		if err != nil {
			return nil, err
		}
		ext, err := z.storeSpan(window[:span], comp)
		if err != nil {
			return nil, err
		}
		zi.extents = append(zi.extents, ext)
		zi.totalBlocks += uint32(ext.blocks)
		window = window[:copy(window, window[span:])]
	}
	return zi, nil
}

// packSpan picks how much of window one pcluster should consume. It probes
// candidate spans with the real compressor, steering by the stream's running
// ratio and bisecting between the largest fitting span and the smallest
// overflowing one. It returns the chosen span and its compressed bytes (nil
// means store the span raw). Probes land in *probe; a fitting result is kept
// by swapping *probe and *kept so later probes never clobber it. Non-final
// spans are lcluster-aligned.
func (z *zState) packSpan(compress func(src, dst []byte) (int, error), window []byte, probe, kept *[]byte, final bool, ratio *float64) (int, []byte, error) {
	bs := z.blockSize
	pc := z.opts.PClusterSize

	// clamp aligns a candidate span: at least one pcluster's worth, at most
	// the window, and lcluster-aligned unless it reaches the window end of a
	// final stream (the EOF tail).
	clamp := func(n int) int {
		if n >= len(window) {
			if final {
				return len(window)
			}
			n = len(window)
		}
		if n < pc {
			n = min(pc, len(window))
		}
		if aligned := n &^ (bs - 1); aligned > 0 && !(final && n == len(window)) {
			n = aligned
		}
		return n
	}
	// fits reports whether compLen makes span a valid compressed extent:
	// within the pcluster budget and saving at least one block (which also
	// guarantees a NONHEAD slot exists to carry the block count).
	fits := func(span, compLen int) bool {
		return compLen > 0 && compLen <= pc && compLen <= span-bs
	}

	// tooBig == 0 means no overflowing candidate seen yet.
	cand := clamp(int(float64(pc) / max(*ratio, 0.02) * 0.98))
	best, bestLen, tooBig := 0, 0, 0
	for try := 0; try < 5; try++ {
		n, err := compress(window[:cand], *probe)
		if err != nil {
			return 0, nil, err
		}
		if fits(cand, n) {
			best, bestLen = cand, n
			*probe, *kept = *kept, *probe
			if n >= pc*31/32 || cand == len(window) {
				break // pcluster essentially full, or stream exhausted
			}
			var grown int
			if tooBig != 0 {
				grown = clamp((best + tooBig) / 2)
			} else {
				grown = clamp(cand * pc * 31 / 32 / n)
			}
			if grown <= best || (tooBig != 0 && grown >= tooBig) {
				break
			}
			cand = grown
			continue
		}
		if n == 0 { // incompressible at this size
			n = cand
		}
		tooBig = cand
		var shrunk int
		if best != 0 {
			shrunk = clamp((best + tooBig) / 2)
			if shrunk <= best || shrunk >= tooBig {
				break
			}
		} else {
			shrunk = clamp(cand * pc * 15 / 16 / n)
			if shrunk >= cand {
				break
			}
		}
		cand = shrunk
	}
	if best == 0 {
		// Last resort: a single-pcluster input span.
		cand = clamp(pc)
		n, err := compress(window[:cand], *probe)
		if err != nil {
			return 0, nil, err
		}
		if !fits(cand, n) {
			return cand, nil, nil // store raw
		}
		best, bestLen = cand, n
		*probe, *kept = *kept, *probe
	}
	*ratio = 0.75**ratio + 0.25*float64(bestLen)/float64(best)
	return best, (*kept)[:bestLen], nil
}

// refineSpan re-encodes a chosen span with the stronger finalizer, keeping
// the probe encoding when the finalizer does not improve on it. Raw spans
// stay raw: the probe already proved compression cannot save a block.
func refineSpan(finalize func(src, dst []byte) (int, error), span, comp, scratch []byte, blockSize int) ([]byte, error) {
	if finalize == nil || comp == nil {
		return comp, nil
	}
	if len(comp)*4 > len(span)*3 {
		// Barely compressible: the stronger encoder cannot claw back enough
		// to justify a second pass.
		return comp, nil
	}
	n, err := finalize(span, scratch)
	if err != nil {
		return nil, err
	}
	if n > 0 && n < len(comp) && n <= len(span)-blockSize {
		return scratch[:n], nil
	}
	return comp, nil
}

// storeSpan writes one packed span to the compressed spool (or reuses an
// identical earlier one) and returns its extent shape. comp holds the span's
// compressed bytes; empty means store the span raw.
func (z *zState) storeSpan(span, comp []byte) (zExtent, error) {
	var key [sha256.Size]byte
	if z.dedupe != nil {
		key = sha256.Sum256(span)
	}
	return z.storeSpanKeyed(span, comp, key)
}

// storeSpanKeyed is storeSpan with the dedupe key already computed (workers
// hash in parallel; the collector stores single-threaded).
func (z *zState) storeSpanKeyed(span, comp []byte, key [sha256.Size]byte) (zExtent, error) {
	if zSelfCheck && len(comp) > 0 {
		out := make([]byte, len(span))
		n, err := lz4.UncompressBlock(comp, out)
		if err != nil || n != len(span) || !bytes.Equal(out[:n], span) {
			return zExtent{}, fmt.Errorf("erofs: self-check failed: span=%d comp=%d decoded=%d err=%v", len(span), len(comp), n, err)
		}
	}
	if z.dedupe != nil {
		if ext, ok := z.dedupe[key]; ok {
			return ext, nil
		}
	}

	bs := z.blockSize
	lclusters := (len(span) + bs - 1) / bs
	ext := zExtent{lclusters: lclusters, blkOff: z.compBlocks}
	if len(comp) > 0 {
		ext.blocks = (len(comp) + bs - 1) / bs
		// LZ4_0PADDING: compressed data is right-aligned within its blocks;
		// the decompressor skips the leading zeros.
		pad := ext.blocks*bs - len(comp)
		if err := z.writeSpoolBlocks(make([]byte, pad), comp); err != nil {
			return zExtent{}, err
		}
	} else {
		ext.plain = true
		ext.blocks = lclusters
		pad := ext.blocks*bs - len(span)
		if err := z.writeSpoolBlocks(span, make([]byte, pad)); err != nil {
			return zExtent{}, err
		}
	}
	z.compBlocks += uint32(ext.blocks)
	if z.dedupe != nil {
		z.dedupe[key] = ext
	}
	return ext, nil
}

func (z *zState) writeSpoolBlocks(parts ...[]byte) error {
	for _, p := range parts {
		if _, err := z.compSpool.Write(p); err != nil {
			return err
		}
	}
	return nil
}

// --- layout helpers ---

// zTrailingSize returns the metadata bytes that follow the inode core and
// xattr area for a compressed entry: alignment padding, the 8-byte map
// header, and (unless the whole file is a fragment) 8 reserved bytes plus
// one 8-byte index per logical cluster.
func (w *erofsWriter) zTrailingSize(e *erofsEntry) int {
	headerEnd := inodeCoreSize(e) + e.xattrSize // nid*32 is 8-aligned, so this decides the padding
	pad := (8 - headerEnd%8) % 8
	if e.z.wholeFragment {
		return pad + zMapHeaderSize
	}
	nIdx := int((e.size + uint64(w.blockSize) - 1) / uint64(w.blockSize))
	return pad + zMapHeaderSize + zFullIndexExtra + nIdx*zLclusterIdxSize
}

// writeZTrailing emits the map header and lcluster index array for a
// compressed entry. Returns the number of bytes written.
func (w *erofsWriter) writeZTrailing(buf io.Writer, e *erofsEntry) (int, error) {
	headerEnd := inodeCoreSize(e) + e.xattrSize
	pad := (8 - headerEnd%8) % 8
	written := 0
	if pad > 0 {
		if _, err := buf.Write(w.zeroBuf[:pad]); err != nil {
			return written, err
		}
		written += pad
	}

	var hdr [zMapHeaderSize]byte
	if e.z.wholeFragment {
		// The whole file lives in the packed inode: the 8-byte header is
		// (1<<63) | fragment offset, little-endian.
		binary.LittleEndian.PutUint64(hdr[:], 1<<63|e.z.fragOff)
		if _, err := buf.Write(hdr[:]); err != nil {
			return written, err
		}
		return written + zMapHeaderSize, nil
	}

	// h_fragmentoff=0, h_advise=BIG_PCLUSTER_1, h_algorithmtype=lz4 for
	// HEAD1, h_clusterbits=0 (lcluster == block).
	binary.LittleEndian.PutUint16(hdr[4:6], zAdviseBigPcluster1)
	if _, err := buf.Write(hdr[:]); err != nil {
		return written, err
	}
	written += zMapHeaderSize
	if _, err := buf.Write(w.zeroBuf[:zFullIndexExtra]); err != nil {
		return written, err
	}
	written += zFullIndexExtra

	var idx [zLclusterIdxSize]byte
	emit := func(advise uint16, clusterofs uint16, u uint32) error {
		binary.LittleEndian.PutUint16(idx[0:2], advise)
		binary.LittleEndian.PutUint16(idx[2:4], clusterofs)
		binary.LittleEndian.PutUint32(idx[4:8], u)
		_, err := buf.Write(idx[:])
		if err == nil {
			written += zLclusterIdxSize
		}
		return err
	}

	tailOfs := uint16(e.size % uint64(w.blockSize))
	for extIdx, ext := range e.z.extents {
		blkaddr := w.z.compBase + ext.blkOff
		if ext.plain {
			// Raw blocks map one lcluster each. A partial final block is
			// simply short; PLAIN ("shifted") extents copy literally.
			for i := 0; i < ext.lclusters; i++ {
				if err := emit(zLclusterPlain, 0, blkaddr+uint32(i)); err != nil {
					return written, err
				}
			}
			continue
		}
		// The final lcluster of a compressed extent that ends mid-lcluster
		// (only possible at EOF, since chunking is lcluster-aligned) becomes
		// a boundary marker: a PLAIN entry whose clusterofs records where
		// the extent's decompressed data ends.
		marker := extIdx == len(e.z.extents)-1 && tailOfs != 0
		if err := emit(zLclusterHead1, 0, blkaddr); err != nil {
			return written, err
		}
		for i := 1; i < ext.lclusters; i++ {
			if marker && i == ext.lclusters-1 {
				if err := emit(zLclusterPlain, tailOfs, 0); err != nil {
					return written, err
				}
				continue
			}
			d0 := uint32(i)
			if i == 1 {
				d0 = zCblkcntBit | uint32(ext.blocks)
			}
			d1 := uint32(ext.lclusters - i)
			if err := emit(zLclusterNonhead, 0, d0|d1<<16); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// writeCompressedData streams the compressed data region (all pcluster
// images, block-aligned) to the output.
func (w *erofsWriter) writeCompressedData(out io.Writer) error {
	if w.z == nil || w.z.compBlocks == 0 {
		return nil
	}
	size := int64(w.z.compBlocks) * int64(w.blockSize)
	n, err := io.Copy(out, io.NewSectionReader(w.z.compSpool, 0, size))
	if err != nil {
		return fmt.Errorf("erofs: write compressed data: %w", err)
	}
	if n != size {
		return fmt.Errorf("erofs: compressed data short write: %d of %d bytes", n, size)
	}
	return nil
}

// zSuperblockCfgs returns the compression configuration record that follows
// the superblock when COMPR_CFGS is set: an le16 length plus the
// z_erofs_lz4_cfgs payload.
func (w *erofsWriter) zSuperblockCfgs() []byte {
	buf := make([]byte, 2+14)
	binary.LittleEndian.PutUint16(buf[0:2], 14)
	binary.LittleEndian.PutUint16(buf[2:4], 65535) // lz4 max match distance
	binary.LittleEndian.PutUint16(buf[4:6], uint16(w.z.opts.PClusterSize/w.blockSize))
	return buf
}

// zSegmentSize is the unit of parallel packing for large streams. Segment
// boundaries force an extent break, costing at most one underfilled pcluster
// per segment — noise at 8 MiB.
const zSegmentSize = 8 << 20

// packedSegment is one segment's packing result, produced by a worker.
type packedSegment struct {
	spans []packedSpan
	err   error
}

// packedSpan is one extent-to-be: the input span it covers and its
// compressed bytes (nil comp means store raw).
type packedSpan struct {
	raw  []byte
	comp []byte
	key  [sha256.Size]byte
}

// compressParallel packs a large stream by splitting it into segments packed
// concurrently, then collects the results in order so extent layout, spool
// contents, and dedupe behavior stay deterministic. In-flight segments are
// bounded by a semaphore the collector replenishes, so memory stays at
// O(workers) segments regardless of stream size.
func (z *zState) compressParallel(r io.Reader, size uint64) (*zInfo, error) {
	segments := int((size + zSegmentSize - 1) / zSegmentSize)
	type segJob struct {
		idx int
		buf []byte
	}
	jobs := make(chan segJob)
	results := make([]chan packedSegment, segments)
	for i := range results {
		results[i] = make(chan packedSegment, 1)
	}
	inflight := make(chan struct{}, z.workers*2)

	// Reader: sequential, in segment order, gated by the in-flight budget.
	var readErr error
	go func() {
		defer close(jobs)
		for idx := 0; idx < segments; idx++ {
			inflight <- struct{}{}
			segLen := int(min(zSegmentSize, size-uint64(idx)*zSegmentSize))
			buf := make([]byte, segLen)
			if _, err := io.ReadFull(r, buf); err != nil {
				readErr = err
				results[idx] <- packedSegment{err: err}
				return
			}
			jobs <- segJob{idx: idx, buf: buf}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < z.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			compress := z.newCompressor()
			var finalize func(src, dst []byte) (int, error)
			if z.newFinalizer != nil {
				finalize = z.newFinalizer()
			}
			probe := make([]byte, lz4.CompressBlockBound(zMaxSpan))
			kept := make([]byte, lz4.CompressBlockBound(zMaxSpan))
			for job := range jobs {
				final := job.idx == segments-1
				ratio := 0.5
				var spans []packedSpan
				window := job.buf
				var err error
				for len(window) > 0 {
					var span int
					var comp []byte
					span, comp, err = z.packSpan(compress, window, &probe, &kept, final, &ratio)
					if err != nil {
						break
					}
					comp, err = refineSpan(finalize, window[:span], comp, probe, z.blockSize)
					if err != nil {
						break
					}
					ps := packedSpan{raw: window[:span], comp: append([]byte(nil), comp...)}
					if z.dedupe != nil {
						ps.key = sha256.Sum256(ps.raw)
					}
					spans = append(spans, ps)
					window = window[span:]
				}
				results[job.idx] <- packedSegment{spans: spans, err: err}
			}
		}()
	}

	zi := &zInfo{}
	var firstErr error
	for idx := 0; idx < segments; idx++ {
		res := <-results[idx]
		<-inflight
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		if firstErr != nil {
			if readErr != nil {
				break // the reader stopped; later segments never arrive
			}
			continue // drain remaining segments
		}
		for _, ps := range res.spans {
			ext, err := z.storeSpanKeyed(ps.raw, ps.comp, ps.key)
			if err != nil {
				firstErr = err
				break
			}
			zi.extents = append(zi.extents, ext)
			zi.totalBlocks += uint32(ext.blocks)
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return zi, nil
}
