package erofs

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"

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
	compressor func(src, dst []byte) (int, error)
}

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
		var c lz4.Compressor
		z.compressor = c.CompressBlock
	case "lz4hc":
		c := lz4.CompressorHC{Level: lz4.Level9}
		z.compressor = c.CompressBlock
	default:
		return nil, fmt.Errorf("erofs: unsupported compression algorithm %q (supported: lz4, lz4hc)", opts.Algorithm)
	}
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
// fragment), then builds the packed inode itself.
func (fsys *Writer) compressAll() error {
	z := fsys.z
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
		if z.opts.Fragments && e.size < uint64(z.opts.PClusterSize) {
			off, err := z.addFragment(r, int(e.size))
			if err != nil {
				return fmt.Errorf("erofs: pack %s: %w", e.path, err)
			}
			e.z = &zInfo{wholeFragment: true, fragOff: off}
		} else {
			zi, err := z.compressStream(r, e.size)
			if err != nil {
				return fmt.Errorf("erofs: compress %s: %w", e.path, err)
			}
			e.z = zi
		}
		if c, ok := e.directData.(io.Closer); ok {
			_ = c.Close()
		}
		e.directData = nil
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

// compressStream chunks a file into pcluster-sized spans and compresses each
// independently, storing raw blocks when compression saves less than one
// block. Chunk boundaries are always lcluster-aligned, so extents never start
// mid-lcluster.
func (z *zState) compressStream(r io.Reader, size uint64) (*zInfo, error) {
	zi := &zInfo{}
	src := make([]byte, z.opts.PClusterSize)
	var remaining = size
	for remaining > 0 {
		n := uint64(z.opts.PClusterSize)
		if remaining < n {
			n = remaining
		}
		chunk := src[:n]
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		remaining -= n

		ext, err := z.storeChunk(chunk)
		if err != nil {
			return nil, err
		}
		zi.extents = append(zi.extents, ext)
		zi.totalBlocks += uint32(ext.blocks)
	}
	return zi, nil
}

// storeChunk writes one chunk to the compressed spool (or reuses an identical
// one) and returns its extent shape.
func (z *zState) storeChunk(chunk []byte) (zExtent, error) {
	var key [sha256.Size]byte
	if z.dedupe != nil {
		key = sha256.Sum256(chunk)
		if ext, ok := z.dedupe[key]; ok {
			return ext, nil
		}
	}

	bs := z.blockSize
	lclusters := (len(chunk) + bs - 1) / bs
	dst := make([]byte, lz4.CompressBlockBound(len(chunk)))
	compLen := 0
	// A compressed extent must save at least one full block; single-lcluster
	// chunks therefore always store raw. Saving a block also guarantees a
	// NONHEAD slot exists to carry the compressed block count.
	if lclusters > 1 {
		n, err := z.compressor(chunk, dst)
		if err != nil {
			return zExtent{}, err
		}
		if n > 0 && n <= len(chunk)-bs && n <= (lclusters-1)*bs {
			compLen = n
		}
	}

	ext := zExtent{lclusters: lclusters, blkOff: z.compBlocks}
	if compLen > 0 {
		ext.blocks = (compLen + bs - 1) / bs
		// LZ4_0PADDING: compressed data is right-aligned within its blocks;
		// the decompressor skips the leading zeros.
		pad := ext.blocks*bs - compLen
		if err := z.writeSpoolBlocks(make([]byte, pad), dst[:compLen]); err != nil {
			return zExtent{}, err
		}
	} else {
		ext.plain = true
		ext.blocks = lclusters
		pad := ext.blocks*bs - len(chunk)
		if err := z.writeSpoolBlocks(chunk, make([]byte, pad)); err != nil {
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
