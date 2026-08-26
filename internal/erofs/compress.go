package erofs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sync"

	"orchestrator/internal/erofs/disk"

	"github.com/pierrec/lz4/v4"
)

// CompressionOptions configures z_erofs compressed output. Compressed images
// use the COMPRESSED_FULL (non-compact index) inode layout with 0-padded lz4
// pclusters, matching what mkfs.erofs -E legacy-compress emits and what every
// kernel since 5.4 mounts.
type CompressionOptions struct {
	// Algorithm selects the block compressor: "lz4" (fast encoder) or
	// "lz4hc" (high-compression encoder, identical decompression).
	Algorithm string
	// PClusterSize is the maximum compressed size of a physical cluster.
	// Must be a multiple of the block size, at most 1 MiB.
	// 0 defaults to 65536.
	PClusterSize int
	// MaxExtentSize bounds the decompressed bytes represented by one physical
	// cluster. Smaller extents reduce read amplification for random access;
	// larger extents generally improve compression and sequential throughput.
	// Must be a multiple of the block size, at most 12 MiB. 0 defaults to 4 MiB.
	MaxExtentSize int
	// Fragments packs small regular files whole into the shared packed inode,
	// so they compress together instead of each paying block-alignment padding.
	Fragments bool
	// PackedPClusterSize overrides PClusterSize for the shared packed inode.
	// A smaller value limits the physical I/O caused by random small-file
	// reads without changing the large-file layout. 0 uses PClusterSize.
	PackedPClusterSize int
	// PackedMaxExtentSize overrides MaxExtentSize for the shared packed inode.
	// This gives small-file reads a strict decompression-amplification bound
	// while large files retain larger sequential extents. 0 uses MaxExtentSize.
	PackedMaxExtentSize int
	// Dedupe reuses identical pclusters and fragments across files, keyed by
	// content hash.
	Dedupe bool
	// FragmentOrder optionally lists hot paths in desired packed-inode order.
	// Paths may be absolute or relative to the image root. Unlisted fragments
	// retain deterministic tree order after the listed working set.
	FragmentOrder []string
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
	fragBuf    []byte // reused while hashing and forwarding one fragment
	blockSize  int
	// newPackers returns a probe and optional stronger finalizer for the worker
	// pool. LZ4HC shares one encoder's large hash-chain allocation between the
	// serial probe and finalization phases.
	newPackers     func(maxExtentSize int) (zCompressor, zFinalizer)
	workers        int
	packedWorkersN int
	dataProfile    zProfile
	packedProfile  zProfile
	dataWorkers    chan *zPackWorker
	packedWorkers  chan *zPackWorker
	segmentBufs    chan []byte
	dataCompBufs   chan []byte
	packedCompBufs chan []byte
}

// zProfile controls the two independent costs of a compressed extent: bytes
// fetched from storage and bytes inflated to satisfy a read.
type zProfile struct {
	pclusterSize  int
	maxExtentSize int
}

type zCompressor func(src, dst []byte) (int, error)
type zFinalizer func(src, dst []byte, limit int) (int, bool, error)

// zPackWorker owns the relatively large encoder tables and scratch buffers
// needed to pack one extent. Keeping a bounded pool per profile lets the
// packed stream, regular-file pool, and large-file packer reuse the same
// allocations instead of each phase rebuilding equivalent state.
type zPackWorker struct {
	compress  zCompressor
	finalize  zFinalizer
	probe     []byte
	kept      []byte
	maxExtent int
}

// zSelfCheck decompresses every stored span and compares it against the
// input before it reaches the image — a debugging aid for encoder work,
// too expensive to leave on.
const zSelfCheck = false

const (
	zDefaultPClusterSize  = 65536
	zDefaultMaxExtentSize = 4 << 20
	zFormatMaxExtentSize  = 12 << 20
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
	if opts.MaxExtentSize == 0 {
		opts.MaxExtentSize = zDefaultMaxExtentSize
	}
	if opts.PackedPClusterSize == 0 {
		opts.PackedPClusterSize = opts.PClusterSize
	}
	if opts.PackedMaxExtentSize == 0 {
		opts.PackedMaxExtentSize = opts.MaxExtentSize
	}
	validatePCluster := func(name string, size int) error {
		if size%blockSize != 0 || size <= 0 || size > 1<<20 {
			return fmt.Errorf("erofs: %s %d must be a positive multiple of block size %d, at most 1 MiB", name, size, blockSize)
		}
		return nil
	}
	validateExtent := func(name string, size int) error {
		if size%blockSize != 0 || size <= 0 || size > zFormatMaxExtentSize {
			return fmt.Errorf("erofs: %s %d must be a positive multiple of block size %d, at most 12 MiB", name, size, blockSize)
		}
		return nil
	}
	if err := validatePCluster("pcluster size", opts.PClusterSize); err != nil {
		return nil, err
	}
	if err := validatePCluster("packed pcluster size", opts.PackedPClusterSize); err != nil {
		return nil, err
	}
	if err := validateExtent("max extent size", opts.MaxExtentSize); err != nil {
		return nil, err
	}
	if err := validateExtent("packed max extent size", opts.PackedMaxExtentSize); err != nil {
		return nil, err
	}
	z := &zState{
		opts:          opts,
		blockSize:     blockSize,
		dataProfile:   zProfile{pclusterSize: opts.PClusterSize, maxExtentSize: opts.MaxExtentSize},
		packedProfile: zProfile{pclusterSize: opts.PackedPClusterSize, maxExtentSize: opts.PackedMaxExtentSize},
	}
	switch opts.Algorithm {
	case "", "lz4":
		z.newPackers = func(_ int) (zCompressor, zFinalizer) {
			var c lz4.Compressor
			return c.CompressBlock, nil
		}
	case "lz4hc":
		z.newPackers = func(maxExtentSize int) (zCompressor, zFinalizer) {
			c := &lz4HCEncoder{chain: make([]uint16, maxExtentSize)}
			return c.CompressBlock, c.CompressBlockOptLimit
		}
	default:
		return nil, fmt.Errorf("erofs: unsupported compression algorithm %q (supported: lz4, lz4hc)", opts.Algorithm)
	}
	z.workers = min(runtime.GOMAXPROCS(0), 8)
	z.packedWorkersN = min(runtime.GOMAXPROCS(0), 12)
	maxWorkers := max(z.workers, z.packedWorkersN)
	z.dataWorkers = make(chan *zPackWorker, maxWorkers)
	z.segmentBufs = make(chan []byte, maxWorkers*2)
	z.dataCompBufs = make(chan []byte, maxWorkers*4)
	z.packedCompBufs = make(chan []byte, maxWorkers*4)
	// Packed and normal-file compression are consecutive phases. Share the
	// pool so normal-file workers grow and reuse the packed workers' encoder
	// state instead of retaining a second profile-specific set.
	z.packedWorkers = z.dataWorkers
	if opts.Dedupe {
		z.dedupe = make(map[[sha256.Size]byte]zExtent)
		z.fragDedupe = make(map[[sha256.Size]byte]uint64)
	}
	var err error
	if z.compSpool, err = os.CreateTemp(tempDir, "erofs-comp-*"); err != nil {
		return nil, err
	}
	os.Remove(z.compSpool.Name())
	return z, nil
}

func (z *zState) packWorkerPool(profile zProfile) chan *zPackWorker {
	if profile == z.packedProfile {
		return z.packedWorkers
	}
	return z.dataWorkers
}

func (z *zState) workerCount(profile zProfile) int {
	if profile == z.packedProfile {
		return z.packedWorkersN
	}
	return z.workers
}

func (z *zState) getPackWorker(profile zProfile) *zPackWorker {
	pool := z.packWorkerPool(profile)
	select {
	case worker := <-pool:
		if worker.maxExtent >= profile.maxExtentSize {
			return worker
		}
	default:
	}
	compress, finalize := z.newPackers(profile.maxExtentSize)
	scratchSize := lz4.CompressBlockBound(profile.maxExtentSize)
	return &zPackWorker{
		compress:  compress,
		finalize:  finalize,
		probe:     make([]byte, scratchSize),
		kept:      make([]byte, scratchSize),
		maxExtent: profile.maxExtentSize,
	}
}

func (z *zState) putPackWorker(profile zProfile, worker *zPackWorker) {
	z.packWorkerPool(profile) <- worker
}

func (z *zState) getSegmentBuffer() []byte {
	select {
	case buf := <-z.segmentBufs:
		return buf[:0]
	default:
		return make([]byte, 0, zSegmentSize)
	}
}

func (z *zState) putSegmentBuffer(buf []byte) {
	if buf == nil {
		return
	}
	select {
	case z.segmentBufs <- buf[:0]:
	default:
	}
}

func (z *zState) compBufferPool(profile zProfile) chan []byte {
	if profile == z.packedProfile {
		return z.packedCompBufs
	}
	return z.dataCompBufs
}

// stageCompressed copies a completed encoding out of worker-owned scratch.
// The returned buffer belongs to one packedSpan until its ordered collector
// has stored (or deduped) that span and calls releaseCompressed.
func (z *zState) stageCompressed(profile zProfile, comp []byte) []byte {
	if len(comp) == 0 {
		return nil
	}
	pool := z.compBufferPool(profile)
	select {
	case buf := <-pool:
		if cap(buf) >= len(comp) {
			return append(buf[:0], comp...)
		}
	default:
	}
	return append(make([]byte, 0, profile.pclusterSize), comp...)
}

func (z *zState) releaseCompressed(profile zProfile, comp []byte) {
	if comp == nil {
		return
	}
	select {
	case z.compBufferPool(profile) <- comp[:0]:
	default:
	}
}

func (z *zState) releasePackedSpans(profile zProfile, spans []packedSpan) {
	for i := range spans {
		z.releaseCompressed(profile, spans[i].comp)
		spans[i].comp = nil
	}
}

func (z *zState) close() {
	if z.compSpool != nil {
		z.compSpool.Close()
	}
}

// compressAll runs the compression pass over every eligible regular file.
// Small files feed the packed-inode compressor as they are read, overlapping
// source I/O with compression and avoiding a raw packed-inode temporary file.
// Remaining mid-size files fan out to a worker pool (collected in tree order,
// so output stays deterministic); large files use the segment-parallel packer.
func (fsys *Writer) compressAll() error {
	z := fsys.z
	entries := collectRegular(fsys.root)
	remaining := make([]*fsEntry, 0, len(entries))

	readerFor := func(e *fsEntry) io.Reader {
		if e.directData != nil {
			return e.directData
		}
		if fsys.spool != nil {
			return io.NewSectionReader(fsys.spool, e.spoolOff, int64(e.size))
		}
		return nil
	}
	closeDirect := func(e *fsEntry) {
		if closer, ok := e.directData.(io.Closer); ok {
			_ = closer.Close()
		}
		e.directData = nil
	}

	// Build the packed inode first. Its segments are compressed concurrently
	// while this goroutine reads, hashes, and concatenates source fragments.
	if z.opts.Fragments {
		packed := newZStreamPacker(z, z.packedProfile)
		defer packed.Close()
		fragments := make([]*fsEntry, 0, len(entries))
		for _, e := range entries {
			if e.size >= 4*uint64(z.opts.PClusterSize) {
				remaining = append(remaining, e)
				continue
			}
			fragments = append(fragments, e)
		}
		var fragmentSizeCounts map[uint64]int
		if z.fragDedupe != nil {
			fragmentSizeCounts = make(map[uint64]int)
			for _, e := range fragments {
				fragmentSizeCounts[e.size]++
			}
			var maxDedupeSize uint64
			for _, e := range fragments {
				if fragmentSizeCounts[e.size] > 1 {
					maxDedupeSize = max(maxDedupeSize, e.size)
				}
			}
			if maxDedupeSize > 0 {
				z.fragBuf = make([]byte, int(maxDedupeSize))
			}
		}
		if len(z.opts.FragmentOrder) > 0 {
			byPath := make(map[string]*fsEntry, len(fragments))
			for _, e := range fragments {
				byPath[e.path] = e
			}
			ordered := make([]*fsEntry, 0, len(fragments))
			for _, hotPath := range z.opts.FragmentOrder {
				hotPath = path.Clean("/" + hotPath)
				if e := byPath[hotPath]; e != nil {
					ordered = append(ordered, e)
					delete(byPath, hotPath)
				}
			}
			for _, e := range fragments {
				if byPath[e.path] != nil {
					ordered = append(ordered, e)
				}
			}
			fragments = ordered
		}
		for _, e := range fragments {
			r := readerFor(e)
			if r == nil {
				continue
			}
			var off uint64
			var err error
			if z.fragDedupe == nil || fragmentSizeCounts[e.size] == 1 {
				// Equal content implies equal size. A globally unique-sized
				// fragment cannot dedupe, so stream it straight into the packed
				// tail instead of hashing and copying it through fragBuf.
				off = z.packedSize
				err = packed.ReadFull(r, int(e.size))
				if err == nil {
					z.packedSize += e.size
				}
			} else {
				off, err = z.addFragment(r, packed, int(e.size))
			}
			if err != nil {
				return fmt.Errorf("erofs: pack %s: %w", e.path, err)
			}
			e.z = &zInfo{wholeFragment: true, fragOff: off}
			closeDirect(e)
		}
		zi, err := packed.Finish()
		if err != nil {
			return fmt.Errorf("erofs: compress packed inode: %w", err)
		}
		if z.packedSize > 0 {
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
	} else {
		remaining = entries
	}

	type fileResult struct {
		spans []packedSpan
		err   error
	}
	type fileJob struct {
		e          *fsEntry
		buf        []byte
		res        chan fileResult
		duplicates []*fsEntry
	}
	jobs := make(chan *fileJob)
	var fileDedupe map[[sha256.Size]byte]*fileJob
	var fileDedupeSizes map[uint64]int
	if z.dedupe != nil {
		fileDedupe = make(map[[sha256.Size]byte]*fileJob)
		fileDedupeSizes = make(map[uint64]int)
		for _, e := range remaining {
			if e.size >= 4*uint64(z.opts.PClusterSize) && e.size < 2*zSegmentSize {
				fileDedupeSizes[e.size]++
			}
		}
	}
	freeFileBuffers := make([][]byte, 0, z.workers*2)
	getFileBuffer := func(size int) []byte {
		best := -1
		for i, buf := range freeFileBuffers {
			if cap(buf) >= size && (best < 0 || cap(buf) < cap(freeFileBuffers[best])) {
				best = i
			}
		}
		if best < 0 {
			return make([]byte, size)
		}
		buf := freeFileBuffers[best]
		freeFileBuffers[best] = freeFileBuffers[len(freeFileBuffers)-1]
		freeFileBuffers = freeFileBuffers[:len(freeFileBuffers)-1]
		return buf[:size]
	}
	putFileBuffer := func(buf []byte) {
		buf = buf[:0]
		if len(freeFileBuffers) < cap(freeFileBuffers) {
			freeFileBuffers = append(freeFileBuffers, buf)
			return
		}
		smallest := 0
		for i := 1; i < len(freeFileBuffers); i++ {
			if cap(freeFileBuffers[i]) < cap(freeFileBuffers[smallest]) {
				smallest = i
			}
		}
		if cap(buf) > cap(freeFileBuffers[smallest]) {
			freeFileBuffers[smallest] = buf
		}
	}
	var wg sync.WaitGroup
	for range z.workers {
		wg.Go(func() {
			for job := range jobs {
				worker := z.getPackWorker(z.dataProfile)
				spans, err := z.packBuffer(job.buf, z.dataProfile, worker.compress, worker.finalize, &worker.probe, &worker.kept)
				z.putPackWorker(z.dataProfile, worker)
				job.res <- fileResult{spans: spans, err: err}
			}
		})
	}
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	// collect resolves one finished job into its entry's extent list.
	pending := make([]*fileJob, 0, z.workers*2)
	collect := func(job *fileJob) error {
		res := <-job.res
		defer func() {
			z.releasePackedSpans(z.dataProfile, res.spans)
			putFileBuffer(job.buf)
		}()
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
		for _, duplicate := range job.duplicates {
			duplicate.z = zi
		}
		return nil
	}
	discard := func(job *fileJob) {
		res := <-job.res
		z.releasePackedSpans(z.dataProfile, res.spans)
		putFileBuffer(job.buf)
	}
	// Any source, compressor, or spool error can leave already-submitted jobs
	// behind. Their result buffers still have owners, so receive and release
	// them before the worker pool is stopped.
	defer func() {
		for _, job := range pending {
			discard(job)
		}
	}()
	drain := func() error {
		for len(pending) > 0 {
			job := pending[0]
			pending = pending[:copy(pending, pending[1:])]
			if err := collect(job); err != nil {
				return err
			}
		}
		return nil
	}

	for _, e := range remaining {
		r := readerFor(e)
		if r == nil {
			continue
		}
		switch {
		case e.size >= 2*zSegmentSize:
			// Large file: extents must land in stream order, so drain the
			// pool first; the stream packer parallelizes internally.
			if err := drain(); err != nil {
				return err
			}
			zi, err := z.compressStream(r, e.size, z.dataProfile)
			if err != nil {
				return fmt.Errorf("erofs: compress %s: %w", e.path, err)
			}
			e.z = zi
		default:
			buf := getFileBuffer(int(e.size))
			if _, err := io.ReadFull(r, buf); err != nil {
				return fmt.Errorf("erofs: read %s: %w", e.path, err)
			}
			job := &fileJob{e: e, buf: buf, res: make(chan fileResult, 1)}
			// Extent dedupe would discover a duplicate only after running both
			// compression passes. For substantial whole-file duplicates, hash
			// the already-resident input first and share the canonical zInfo.
			// The cutoff keeps tiny-file workloads from growing a large map;
			// with fragments enabled those files are deduped during packing.
			if fileDedupeSizes[e.size] > 1 {
				key := sha256.Sum256(buf)
				if canonical := fileDedupe[key]; canonical != nil {
					if canonical.e.z != nil {
						e.z = canonical.e.z
					} else {
						canonical.duplicates = append(canonical.duplicates, e)
					}
					putFileBuffer(buf)
					closeDirect(e)
					continue
				}
				fileDedupe[key] = job
			}
			if len(pending) == cap(pending) {
				job := pending[0]
				pending = pending[:copy(pending, pending[1:])]
				if err := collect(job); err != nil {
					return err
				}
			}
			pending = append(pending, job)
			jobs <- job
		}
		closeDirect(e)
	}
	if err := drain(); err != nil {
		return err
	}
	return nil
}

// packBuffer packs one whole in-memory stream into spans, hashing them for
// dedupe when enabled. Used by the per-file worker pool.
func (z *zState) packBuffer(buf []byte, profile zProfile, compress zCompressor, finalize zFinalizer, probe, kept *[]byte) ([]packedSpan, error) {
	var spans []packedSpan
	window := buf
	ratio := 0.5
	for len(window) > 0 {
		candidate := window[:min(len(window), profile.maxExtentSize)]
		final := len(candidate) == len(window)
		span, comp, err := z.packSpan(compress, candidate, profile, probe, kept, final, &ratio)
		if err != nil {
			return spans, err
		}
		comp, err = refineSpan(finalize, window[:span], comp, *probe, z.blockSize)
		if err != nil {
			return spans, err
		}
		ps := packedSpan{raw: window[:span], comp: z.stageCompressed(profile, comp)}
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
func (z *zState) addFragment(r io.Reader, dst io.Writer, size int) (uint64, error) {
	if cap(z.fragBuf) < size {
		z.fragBuf = make([]byte, size)
	}
	buf := z.fragBuf[:size]
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
	if _, err := dst.Write(buf); err != nil {
		return 0, err
	}
	z.packedSize += uint64(size)
	if z.fragDedupe != nil {
		z.fragDedupe[key] = off
	}
	return off, nil
}

// compressStream packs a file into pclusters mkfs.erofs-style: each pcluster
// holds as much input as compresses into the profile's pcluster size (found with a
// ratio-guided search rather than a destsize codec), storing raw blocks when
// compression saves less than one block. Span boundaries are always
// lcluster-aligned, so extents never start mid-lcluster.
func (z *zState) compressStream(r io.Reader, size uint64, profile zProfile) (*zInfo, error) {
	if size >= 2*zSegmentSize && z.workers > 1 {
		return z.compressParallel(r, size, profile)
	}
	zi := &zInfo{}
	worker := z.getPackWorker(profile)
	defer z.putPackWorker(profile, worker)
	window := make([]byte, 0, min(size, uint64(profile.maxExtentSize)))
	remaining := size
	ratio := 0.5 // running compressed/uncompressed estimate for this stream
	for remaining > 0 || len(window) > 0 {
		// Refill the window up to the decompressed-extent limit.
		if want := int(min(uint64(profile.maxExtentSize), uint64(len(window))+remaining)); len(window) < want {
			off := len(window)
			window = window[:want]
			if _, err := io.ReadFull(r, window[off:]); err != nil {
				return nil, err
			}
			remaining -= uint64(want - off)
		}

		final := remaining == 0
		span, comp, err := z.packSpan(worker.compress, window, profile, &worker.probe, &worker.kept, final, &ratio)
		if err != nil {
			return nil, err
		}
		comp, err = refineSpan(worker.finalize, window[:span], comp, worker.probe, z.blockSize)
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
func (z *zState) packSpan(compress zCompressor, window []byte, profile zProfile, probe, kept *[]byte, final bool, ratio *float64) (int, []byte, error) {
	bs := z.blockSize
	pc := profile.pclusterSize

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
		if aligned := n &^ (bs - 1); aligned > 0 && (!final || n != len(window)) {
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
	best, bestLen, tooBig, lastCand := 0, 0, 0, 0
	for range 5 {
		n, err := compress(window[:cand], *probe)
		if err != nil {
			return 0, nil, err
		}
		lastCand = cand
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
		if cand == lastCand {
			return cand, nil, nil // the identical failed probe already ran
		}
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
func refineSpan(finalize zFinalizer, span, comp, scratch []byte, blockSize int) ([]byte, error) {
	if finalize == nil || comp == nil {
		return comp, nil
	}
	if len(comp)*4 > len(span)*3 {
		// Barely compressible: the stronger encoder cannot claw back enough
		// to justify a second pass.
		return comp, nil
	}
	fastBlocks := (len(comp) + blockSize - 1) / blockSize
	if fastBlocks == 1 {
		return comp, nil // no stronger encoding can occupy fewer blocks
	}
	n, withinLimit, err := finalize(span, scratch, (fastBlocks-1)*blockSize)
	if err != nil {
		return nil, err
	}
	if !withinLimit {
		return comp, nil
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
			return zExtent{}, fmt.Errorf("erofs: self-check failed: span=%d comp=%d decoded=%d err=%w", len(span), len(comp), n, err)
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
			for i := range ext.lclusters {
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
			} else if d0 >= zCblkcntBit {
				// The flag bit is reserved for CBLKCNT. Long extents use
				// bounded backward hops, as mkfs.erofs does, rather than
				// letting a large HEAD distance alias the flag.
				d0 = zCblkcntBit - 1
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
	maxPClusterSize := max(w.z.dataProfile.pclusterSize, w.z.packedProfile.pclusterSize)
	binary.LittleEndian.PutUint16(buf[4:6], uint16(maxPClusterSize/w.blockSize))
	return buf
}

// zSegmentSize is the unit of parallel packing for large streams. Segment
// boundaries force an extent break, costing at most one underfilled pcluster
// per segment — noise at 8 MiB.
const zSegmentSize = 8 << 20

// packedSegment is one segment's packing result, produced by a worker.
type packedSegment struct {
	spans []packedSpan
	buf   []byte
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
func (z *zState) compressParallel(r io.Reader, size uint64, profile zProfile) (*zInfo, error) {
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
		for idx := range segments {
			inflight <- struct{}{}
			segLen := int(min(zSegmentSize, size-uint64(idx)*zSegmentSize))
			buf := z.getSegmentBuffer()[:segLen]
			if _, err := io.ReadFull(r, buf); err != nil {
				readErr = err
				results[idx] <- packedSegment{buf: buf, err: err}
				return
			}
			jobs <- segJob{idx: idx, buf: buf}
		}
	}()

	var wg sync.WaitGroup
	for range z.workers {
		wg.Go(func() {
			for job := range jobs {
				worker := z.getPackWorker(profile)
				final := job.idx == segments-1
				ratio := 0.5
				var spans []packedSpan
				window := job.buf
				var err error
				for len(window) > 0 {
					candidate := window[:min(len(window), profile.maxExtentSize)]
					var span int
					var comp []byte
					span, comp, err = z.packSpan(worker.compress, candidate, profile, &worker.probe, &worker.kept, final && len(candidate) == len(window), &ratio)
					if err != nil {
						break
					}
					comp, err = refineSpan(worker.finalize, window[:span], comp, worker.probe, z.blockSize)
					if err != nil {
						break
					}
					ps := packedSpan{raw: window[:span], comp: z.stageCompressed(profile, comp)}
					if z.dedupe != nil {
						ps.key = sha256.Sum256(ps.raw)
					}
					spans = append(spans, ps)
					window = window[span:]
				}
				z.putPackWorker(profile, worker)
				results[job.idx] <- packedSegment{spans: spans, buf: job.buf, err: err}
			}
		})
	}

	zi := &zInfo{}
	var firstErr error
	for idx := range segments {
		res := <-results[idx]
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		if firstErr != nil {
			z.releasePackedSpans(profile, res.spans)
			z.putSegmentBuffer(res.buf)
			<-inflight
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
		z.releasePackedSpans(profile, res.spans)
		z.putSegmentBuffer(res.buf)
		<-inflight
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return zi, nil
}
