package erofs

import (
	"orchestrator/internal/erofs/disk"
	"slices"
	"strings"
)

// planLayout assigns NIDs and determines trailing data sizes for all entries.
func (w *erofsWriter) planLayout(root *erofsEntry) {
	align32 := func(n int) int { return (n + 31) &^ 31 }
	alignBlock := func(n int) int { return (n + w.blockSize - 1) &^ (w.blockSize - 1) }
	// Collect all entries in a deterministic order (DFS, pre-order).
	// DFS keeps directory contents close to their parent inode and allows
	// small directories to use inline data with very little padding.
	w.entries = nil
	var walk func(e *erofsEntry)
	walk = func(e *erofsEntry) {
		w.entries = append(w.entries, e)
		if e.mode&disk.StatTypeMask == disk.StatTypeDir {
			slices.SortFunc(e.children, func(a, b *erofsEntry) int {
				return strings.Compare(a.name, b.name)
			})
			for _, c := range e.children {
				walk(c)
			}
		}
	}
	walk(root)
	if w.z != nil && w.z.packed != nil {
		// The packed inode is hidden: reachable only via the superblock's
		// packed_nid, never through a directory.
		w.entries = append(w.entries, w.z.packed)
	}

	w.totalInodes = uint64(len(w.entries))

	// Block 0 holds: 1024-byte pad + 128-byte superblock + device slot(s) + padding
	// MetaBlkAddr is set later by write() depending on the on-disk layout.

	// Assign NIDs sequentially.
	// NID = byte offset from metaStartPos / 32.
	// Each extended inode is 64 bytes = 2 NID slots.
	// Trailing data follows and is padded to 32-byte boundary.
	currentOff := 0 // byte offset from metaStartPos
	for _, e := range w.entries {
		e.nid = uint64(currentOff / 32)
		e.xattrSize = calcXattrSize(e)

		// Decide compact (32B) vs extended (64B) inode.
		e.compact = e.uid <= 0xFFFF && e.gid <= 0xFFFF &&
			e.nlink <= 0xFFFF && e.size <= 0xFFFFFFFF &&
			(w.compactInodes || (e.mtime == w.buildTime && e.mtimeNs == 0))

		inodeSize := disk.SizeInodeExtended
		if e.compact {
			inodeSize = disk.SizeInodeCompact
		}

		// The inode header region is inode core + xattr area.
		// Trailing data (dirents, chunk indexes, inline data) follows.
		headerSize := inodeSize + e.xattrSize

		// Decide layout at the inode's actual offset. The core itself may not
		// cross a metadata block boundary.
		if currentOff%w.blockSize+inodeSize > w.blockSize {
			currentOff = alignBlock(currentOff)
			e.nid = uint64(currentOff / 32)
		}

		// Determine layout
		switch e.mode & disk.StatTypeMask {
		case disk.StatTypeReg:
			switch {
			case e.z != nil:
				e.layout = disk.LayoutCompressedFull
			case e.size == 0 && len(e.chunks) == 0 && e.data == nil && !e.metadataOnly:
				e.layout = disk.LayoutFlatPlain
			case len(e.chunks) > 0 || e.metadataOnly:
				e.layout = disk.LayoutChunkBased
				if e.contiguous {
					e.chunkBits = w.minChunkBits(e.size)
				}
			default:
				// Full-image mode: decide inline vs plain
				if int(e.size) <= w.blockSize-headerSize {
					blockOff := currentOff % w.blockSize
					if blockOff+headerSize+int(e.size) <= w.blockSize {
						e.layout = disk.LayoutFlatInline
					} else {
						e.layout = disk.LayoutFlatPlain
					}
				} else {
					e.layout = disk.LayoutFlatPlain
				}
			}
		case disk.StatTypeDir:
			direntDataSize := w.direntDataSize(e)
			blockOff := currentOff % w.blockSize
			if direntDataSize > 0 && blockOff+headerSize+direntDataSize <= w.blockSize {
				e.layout = disk.LayoutFlatInline
			} else if direntDataSize > 0 && headerSize+direntDataSize <= w.blockSize {
				// If a small directory only misses inline placement because this
				// inode is near the end of a metadata block, advancing to the next
				// block can eliminate a whole external directory-data block. Do it
				// only when padding plus inline metadata is no larger overall.
				nextBlock := alignBlock(currentOff)
				padding := nextBlock - currentOff
				inlineMeta := align32(headerSize + direntDataSize)
				plainMeta := align32(headerSize)
				externalData := alignBlock(direntDataSize)
				if padding+inlineMeta <= plainMeta+externalData {
					currentOff = nextBlock
					e.nid = uint64(currentOff / 32)
					e.layout = disk.LayoutFlatInline
				} else {
					e.layout = disk.LayoutFlatPlain
				}
			} else {
				e.layout = disk.LayoutFlatPlain
			}
		case disk.StatTypeSymlink:
			blockOff := currentOff % w.blockSize
			if len(e.symTarget) > 0 && blockOff+headerSize+len(e.symTarget) <= w.blockSize {
				e.layout = disk.LayoutFlatInline
			} else {
				e.layout = disk.LayoutFlatPlain
			}
		default:
			// Device files, fifos, sockets
			e.layout = disk.LayoutFlatPlain
		}

		// Recalculate trailing size now that layout is decided
		e.trailingSize = w.calcTrailingSize(e)

		totalInodeSize := headerSize + e.trailingSize
		// Pad to 32-byte boundary
		if totalInodeSize%32 != 0 {
			totalInodeSize = (totalInodeSize + 31) & ^31
		}

		// Also check that trailing data doesn't cross block boundary for inline layouts
		if e.layout == disk.LayoutFlatInline {
			blockOff := currentOff % w.blockSize
			if blockOff+headerSize+e.trailingSize > w.blockSize {
				// Fall back to flat-plain (data would cross block boundary)
				e.layout = disk.LayoutFlatPlain
				e.trailingSize = w.calcTrailingSize(e)
				totalInodeSize = headerSize + e.trailingSize
				if totalInodeSize%32 != 0 {
					totalInodeSize = (totalInodeSize + 31) & ^31
				}
			}
		}

		currentOff += totalInodeSize
	}

	w.rootNid = root.nid
}

// calcTrailingSize returns the number of bytes following the 64-byte inode.
func (w *erofsWriter) calcTrailingSize(e *erofsEntry) int {
	switch e.mode & disk.StatTypeMask {
	case disk.StatTypeReg:
		if e.layout == disk.LayoutCompressedFull {
			return w.zTrailingSize(e)
		}
		if e.layout == disk.LayoutChunkBased {
			if e.size == 0 && len(e.chunks) == 0 {
				return 0
			}
			cs := w.entryChunkSize(e)
			nchunks := (int(e.size) + cs - 1) / cs
			return nchunks * disk.SizeChunkIndex
		}
		if e.layout == disk.LayoutFlatInline {
			return int(e.size)
		}
		return 0
	case disk.StatTypeDir:
		if e.layout == disk.LayoutFlatInline {
			return w.direntDataSize(e)
		}
		return 0
	case disk.StatTypeSymlink:
		if e.layout == disk.LayoutFlatInline {
			return len(e.symTarget)
		}
		return 0
	default:
		return 0
	}
}

// direntNames returns the sorted list of dirent names for a directory,
// including "." and "..". EROFS requires dirents within each block to
// be sorted alphabetically.
func direntNames(e *erofsEntry) []string {
	names := make([]string, 0, len(e.children)+2)
	names = append(names, ".", "..")
	for _, c := range e.children {
		names = append(names, c.name)
	}
	slices.Sort(names)
	return names
}

// direntDataSize calculates the serialized EROFS dirent data size for a directory.
// For multi-block directories, this includes inter-block padding.
func (w *erofsWriter) direntDataSize(e *erofsEntry) int {
	names := direntNames(e)
	nEntries := len(names)
	if len(e.children) == 0 {
		// Empty dir still needs "." and ".." entries
		return 2*disk.SizeDirent + 1 + 2
	}

	totalSize := 0
	i := 0
	for i < nEntries {
		blockUsed := 0
		start := i
		nameSize := 0
		for j := i; j < nEntries; j++ {
			headerSize := (j - start + 1) * disk.SizeDirent
			nameSize += len(names[j])
			needed := headerSize + nameSize
			if needed > w.blockSize {
				break
			}
			blockUsed = needed
			i = j + 1
		}
		if i == start {
			blockUsed = disk.SizeDirent + len(names[i])
			i++
		}
		// Pad non-final blocks to block boundary
		if i < nEntries && blockUsed%w.blockSize != 0 {
			blockUsed = (blockUsed + w.blockSize - 1) & ^(w.blockSize - 1)
		}
		totalSize += blockUsed
	}

	return totalSize
}
