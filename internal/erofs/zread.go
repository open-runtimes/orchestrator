package erofs

import (
	"encoding/binary"
	"fmt"

	"github.com/pierrec/lz4/v4"
	"orchestrator/internal/erofs/disk"
)

// Read support for z_erofs compressed inodes, covering the subset this
// package writes: COMPRESSED_FULL (non-compact) lcluster indexes, lz4 with
// 0-padded (big) pclusters, and packed-inode fragments including whole-file
// packing. Compact indexes and other algorithms remain unsupported.

// Format-level pcluster bounds (Z_EROFS_PCLUSTER_MAX_SIZE and
// Z_EROFS_PCLUSTER_MAX_DSIZE in the kernel): the compressed and decompressed
// size any one physical cluster may reach. Reads reject indexes beyond them
// before allocating.
const (
	zMaxPClusterSize  = 1 << 20
	zMaxPClusterDSize = 12 << 20
)

// zRead is the lazily-parsed compressed-layout state of one inode, plus a
// one-extent decompression cache: module-style access decompresses each
// pcluster once for a sequential read instead of once per block.
type zRead struct {
	wholeFragment bool
	fragOff       uint64
	idxStart      int64 // absolute byte offset of the lcluster index array
	nIdx          int

	// cached decompressed extent
	extStartLcn int
	extData     []byte
}

// zSupported reports whether the compressed image uses only features this
// reader implements.
func zSupported(sb *disk.SuperBlock) bool {
	const supported = disk.FeatureIncompatLZ4_0Padding | disk.FeatureIncompatComprCfgs |
		disk.FeatureIncompatChunkedFile | disk.FeatureIncompatDeviceTable |
		disk.FeatureIncompatFragments | disk.FeatureIncompatXattrPrefixes
	if sb.FeatureIncompat&^uint32(supported) != 0 {
		return false
	}
	return sb.ComprAlgs == 0 || sb.ComprAlgs == 1<<disk.ComprAlgLZ4
}

// zInit parses the 8-byte map header that follows the inode core and xattr
// area of a compressed inode.
func (img *image) zInit(fi *inode) error {
	if fi.z != nil {
		return nil
	}
	pos := img.metaStartPos() + int64(fi.nid*disk.SizeInodeCompact) + fi.flatDataOffset()
	pos = (pos + 7) &^ 7
	var hdr [8]byte
	if _, err := img.meta.ReadAt(hdr[:], pos); err != nil {
		return fmt.Errorf("read z map header for nid %d: %w", fi.nid, err)
	}
	z := &zRead{extStartLcn: -1}
	raw := binary.LittleEndian.Uint64(hdr[:])
	if raw>>63 != 0 {
		// Whole file stored in the packed inode; the remaining bits are the
		// fragment offset.
		z.wholeFragment = true
		z.fragOff = raw &^ (1 << 63)
		fi.z = z
		return nil
	}
	advise := binary.LittleEndian.Uint16(hdr[4:6])
	algo := hdr[6]
	clusterbits := hdr[7]
	if algo&0xf != disk.ComprAlgLZ4 || clusterbits&0xf != 0 {
		return fmt.Errorf("unsupported z inode (advise=0x%x algo=%d clusterbits=%d) for nid %d: %w",
			advise, algo, clusterbits, fi.nid, ErrNotImplemented)
	}
	z.idxStart = pos + zMapHeaderSize + zFullIndexExtra
	z.nIdx = int((fi.size + int64(img.blockSize()) - 1) >> img.sb.BlkSizeBits)
	fi.z = z
	return nil
}

// zIndex reads one on-disk lcluster index.
func (img *image) zIndex(fi *inode, lcn int) (advise, clusterofs uint16, u uint32, err error) {
	if lcn < 0 || lcn >= fi.z.nIdx {
		return 0, 0, 0, fmt.Errorf("lcluster %d out of range for nid %d: %w", lcn, fi.nid, ErrInvalid)
	}
	var buf [zLclusterIdxSize]byte
	if _, err := img.meta.ReadAt(buf[:], fi.z.idxStart+int64(lcn)*zLclusterIdxSize); err != nil {
		return 0, 0, 0, err
	}
	return binary.LittleEndian.Uint16(buf[0:2]),
		binary.LittleEndian.Uint16(buf[2:4]),
		binary.LittleEndian.Uint32(buf[4:8]), nil
}

// zLoadBlock returns the logical block containing pos for a compressed inode.
func (img *image) zLoadBlock(fi *inode, pos int64) (*block, error) {
	if err := img.zInit(fi); err != nil {
		return nil, err
	}
	blockSize := int64(img.blockSize())
	blockEnd := blockSize
	bn := pos >> img.sb.BlkSizeBits
	if (bn+1)<<img.sb.BlkSizeBits > fi.size {
		blockEnd = fi.size - bn<<img.sb.BlkSizeBits
	}

	b := img.getBlock()
	b.offset = int32(pos % blockSize)
	b.end = int32(blockEnd)

	var err error
	if fi.z.wholeFragment {
		err = img.zReadPacked(fi.z.fragOff+uint64(bn<<img.sb.BlkSizeBits), b.buf[:blockEnd])
	} else {
		err = img.zReadIndexed(fi, bn, b.buf[:blockEnd])
	}
	if err != nil {
		img.putBlock(b)
		return nil, err
	}
	return b, nil
}

// zReadPacked reads bytes from the packed inode's decompressed data.
func (img *image) zReadPacked(off uint64, dst []byte) error {
	packedNid := img.sb.PackedNid
	if packedNid == 0 {
		return fmt.Errorf("fragment read without a packed inode: %w", ErrInvalid)
	}
	f := &file{img: img, name: "(packed)", nid: packedNid}
	fi, err := f.readInfo()
	if err != nil {
		return fmt.Errorf("read packed inode: %w", err)
	}
	for len(dst) > 0 {
		if int64(off) >= fi.size {
			return fmt.Errorf("fragment read past packed inode end: %w", ErrInvalid)
		}
		blk, err := img.loadBlock(fi, int64(off))
		if err != nil {
			return err
		}
		n := copy(dst, blk.bytes())
		img.putBlock(blk)
		dst = dst[n:]
		off += uint64(n)
	}
	return nil
}

// zReadIndexed fills dst with logical block bn of an indexed compressed
// inode, decompressing (and caching) the extent that contains it.
func (img *image) zReadIndexed(fi *inode, bn int64, dst []byte) error {
	lcn := int(bn)

	// Walk back to the extent HEAD.
	head := lcn
	advise, clusterofs, u, err := img.zIndex(fi, head)
	if err != nil {
		return err
	}
	for advise&0x3 == zLclusterNonhead {
		d0 := u & 0xffff
		if d0&zCblkcntBit != 0 {
			d0 = 1
		}
		if d0 == 0 {
			return fmt.Errorf("bogus NONHEAD delta at lcn %d for nid %d: %w", head, fi.nid, ErrInvalid)
		}
		head -= int(d0)
		if advise, clusterofs, u, err = img.zIndex(fi, head); err != nil {
			return err
		}
	}
	// A boundary marker (PLAIN, no block) terminates the previous extent;
	// data before its clusterofs belongs to the extent behind it.
	if advise&0x3 == zLclusterPlain && u == 0 && clusterofs != 0 {
		head--
		if advise, _, u, err = img.zIndex(fi, head); err != nil {
			return err
		}
		for advise&0x3 == zLclusterNonhead {
			d0 := u & 0xffff
			if d0&zCblkcntBit != 0 {
				d0 = 1
			}
			head -= int(d0)
			if advise, _, u, err = img.zIndex(fi, head); err != nil {
				return err
			}
		}
	}

	switch advise & 0x3 {
	case zLclusterPlain:
		// Raw stored block; map one-to-one.
		off := (int64(lcn) - int64(head)) << img.sb.BlkSizeBits
		_, err := img.meta.ReadAt(dst, int64(u)<<img.sb.BlkSizeBits+off)
		return err
	case zLclusterHead1:
		data, err := img.zExtentData(fi, head, u)
		if err != nil {
			return err
		}
		start := (int64(lcn) - int64(head)) << img.sb.BlkSizeBits
		if start >= int64(len(data)) || int(start)+len(dst) > len(data) {
			return fmt.Errorf("extent at lcn %d too short for block %d of nid %d: %w", head, lcn, fi.nid, ErrInvalid)
		}
		copy(dst, data[start:])
		return nil
	default:
		return fmt.Errorf("unexpected lcluster type %d at lcn %d for nid %d: %w", advise&0x3, head, fi.nid, ErrInvalid)
	}
}

// zExtentData decompresses (or returns the cached copy of) the extent whose
// HEAD lcluster is head, stored at block address blkaddr.
func (img *image) zExtentData(fi *inode, head int, blkaddr uint32) ([]byte, error) {
	z := fi.z
	if z.extStartLcn == head {
		return z.extData, nil
	}

	// Scan forward to size the extent and find the compressed block count.
	blockSize := int64(img.blockSize())
	blocks := 1
	lclusters := 1
	decompressed := int64(0)
	for i := head + 1; i < z.nIdx; i++ {
		advise, clusterofs, u, err := img.zIndex(fi, i)
		if err != nil {
			return nil, err
		}
		if advise&0x3 == zLclusterNonhead {
			d0 := u & 0xffff
			if i == head+1 && d0&zCblkcntBit != 0 {
				blocks = int(d0 &^ zCblkcntBit)
			}
			lclusters++
			continue
		}
		if advise&0x3 == zLclusterPlain && u == 0 && clusterofs != 0 {
			// EOF boundary marker: the extent ends clusterofs bytes into
			// this lcluster.
			decompressed = int64(lclusters)<<img.sb.BlkSizeBits + int64(clusterofs)
		}
		break
	}
	if decompressed == 0 {
		decompressed = int64(lclusters) << img.sb.BlkSizeBits
		if end := int64(head)<<img.sb.BlkSizeBits + decompressed; end > fi.size {
			decompressed = fi.size - int64(head)<<img.sb.BlkSizeBits
		}
	}
	// Both sizes come from untrusted on-disk indexes; bound them by the
	// format's pcluster limits before allocating, so a crafted image cannot
	// demand arbitrary memory.
	if int64(blocks)*blockSize > zMaxPClusterSize {
		return nil, fmt.Errorf("pcluster at block %d for nid %d spans %d blocks, exceeding the format limit: %w",
			blkaddr, fi.nid, blocks, ErrInvalid)
	}
	if decompressed <= 0 || decompressed > zMaxPClusterDSize {
		return nil, fmt.Errorf("pcluster at block %d for nid %d decodes to %d bytes, exceeding the format limit: %w",
			blkaddr, fi.nid, decompressed, ErrInvalid)
	}

	comp := make([]byte, int64(blocks)*blockSize)
	if _, err := img.meta.ReadAt(comp, int64(blkaddr)*blockSize); err != nil {
		return nil, err
	}
	// LZ4_0PADDING: compressed data is right-aligned; skip leading zeros.
	i := 0
	for i < len(comp) && comp[i] == 0 {
		i++
	}
	if i == len(comp) {
		return nil, fmt.Errorf("empty pcluster at block %d for nid %d: %w", blkaddr, fi.nid, ErrInvalid)
	}

	out := make([]byte, decompressed)
	n, err := lz4.UncompressBlock(comp[i:], out)
	if err != nil {
		return nil, fmt.Errorf("decompress pcluster at block %d for nid %d: %w", blkaddr, fi.nid, err)
	}
	if int64(n) != decompressed {
		return nil, fmt.Errorf("pcluster at block %d for nid %d decompressed to %d bytes, want %d: %w",
			blkaddr, fi.nid, n, decompressed, ErrInvalid)
	}
	z.extStartLcn = head
	z.extData = out
	return out, nil
}
