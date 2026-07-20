package artifact

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"orchestrator/internal/squashfs"
)

// The forked squashfs package (internal/squashfs) leaves its zstd and lz4
// compressors unregistered — it only knows the algorithm IDs. We register the
// codecs here so squashfs zstd and lz4 work in every build, symmetric with tar.
// lz4 also needs the compressor-options superblock record the forked writer
// emits; see the raw-block note on the LZ4 handler below.
func init() {
	squashfs.RegisterCompHandler(squashfs.ZSTD, &squashfs.CompHandler{
		Decompress: squashfs.MakeDecompressor(zstd.ZipDecompressor()),
		Compress: func(buf []byte) ([]byte, error) {
			var out bytes.Buffer
			w, err := zstd.NewWriter(&out)
			if err != nil {
				return nil, err
			}
			if _, err := w.Write(buf); err != nil {
				_ = w.Close()
				return nil, err
			}
			if err := w.Close(); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		},
	})

	// squashfs compresses each block with the raw lz4 *block* format (not the
	// self-framed stream lz4.NewWriter/NewReader produce) — that's what the
	// kernel driver and unsquashfs expect, paired with the compressor-options
	// record the forked writer emits.
	squashfs.RegisterCompHandler(squashfs.LZ4, &squashfs.CompHandler{
		Decompress: func(buf []byte) ([]byte, error) {
			// A squashfs block never exceeds the 1 MiB format maximum, so a
			// buffer of that size always holds the decompressed output.
			dst := make([]byte, 1<<20)
			n, err := lz4.UncompressBlock(buf, dst)
			if err != nil {
				return nil, err
			}
			return dst[:n], nil
		},
		Compress: func(buf []byte) ([]byte, error) {
			dst := make([]byte, lz4.CompressBlockBound(len(buf)))
			var c lz4.Compressor
			n, err := c.CompressBlock(buf, dst)
			if err != nil {
				return nil, err
			}
			// n == 0 means incompressible; return the input so the writer stores
			// the block uncompressed (len(compressed) < len(block) is false).
			if n == 0 {
				return buf, nil
			}
			return dst[:n], nil
		},
	})
}

// squashfsMagic is the 4-byte signature at the start of every squashfs image.
const squashfsMagic = "hsqs"

// isSquashfs reports whether b starts with the squashfs signature.
func isSquashfs(b []byte) bool {
	return len(b) >= 4 && string(b[:4]) == squashfsMagic
}

// squashfsCompression maps a compression name to a squashfs algorithm. An empty
// name defaults to gzip — squashfs is always compressed. zstd and lz4 are
// registered by init() above.
func squashfsCompression(name string) (squashfs.Compression, error) {
	switch name {
	case "", "gzip":
		return squashfs.GZip, nil
	case "zstd":
		return squashfs.ZSTD, nil
	case "lz4":
		return squashfs.LZ4, nil
	default:
		return 0, fmt.Errorf("unsupported squashfs compression: %q (supported: gzip, zstd, lz4)", name)
	}
}

// writeSquashfs builds a squashfs image at destPath from the file or directory
// at srcPath, using the given compression and block size (bytes).
func writeSquashfs(srcPath, destPath, compression string, blockSize uint32) error {
	comp, err := squashfsCompression(compression)
	if err != nil {
		return err
	}

	info, err := os.Stat(srcPath)
	if err != nil {
		return fmt.Errorf("failed to stat source: %w", err)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer out.Close()

	w, err := squashfs.NewWriter(out, squashfs.WithCompression(comp), squashfs.WithBlockSize(blockSize))
	if err != nil {
		return fmt.Errorf("failed to create squashfs writer: %w", err)
	}

	// AddFS streams each file block-by-block during Finalize. For a single file
	// we wrap it in a one-entry fs.FS rather than AddFile([]byte), which would
	// buffer the whole file in the sidecar's memory and risk an OOM on a large
	// input.
	src := os.DirFS(srcPath)
	if !info.IsDir() {
		src = singleFileFS{fsys: os.DirFS(filepath.Dir(srcPath)), name: filepath.Base(srcPath)}
	}
	if err := w.AddFS(src); err != nil {
		return fmt.Errorf("failed to add source: %w", err)
	}

	if err := w.Finalize(); err != nil {
		return fmt.Errorf("failed to finalize squashfs: %w", err)
	}

	// Pad to a 4 KiB boundary. The kernel reads a squashfs through the loop
	// device in blocks and returns EIO when the image isn't block-aligned;
	// mksquashfs pads for the same reason. The superblock records the real
	// size, so trailing zeros are inert (and the image still reads back fine).
	fi, err := out.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat squashfs: %w", err)
	}
	const align = 4096
	if padded := (fi.Size() + align - 1) / align * align; padded != fi.Size() {
		if err := out.Truncate(padded); err != nil {
			return fmt.Errorf("failed to pad squashfs: %w", err)
		}
	}
	return nil
}

// extractSquashfs extracts a squashfs image at srcPath into destDir. If
// strip is set, the first path component of every entry is dropped. If
// subdir is set, only entries under it are extracted, with the prefix
// stripped. (This is the `unarchive` path; the `mount` artifact mounts the
// image read-only instead of materializing it.)
func extractSquashfs(srcPath, destDir, subdir string, strip bool) error {
	sb, err := squashfs.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open squashfs: %w", err)
	}
	defer sb.Close()

	subdir = cleanSubdir(subdir)

	extracted := 0
	err = fs.WalkDir(sb, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}

		rel := p
		if strip {
			parts := strings.SplitN(p, "/", 2)
			if len(parts) < 2 {
				return nil // the wrapper root directory entry itself
			}
			rel = parts[1]
		}
		if subdir != "" {
			if rel != subdir && !strings.HasPrefix(rel, subdir+"/") {
				return nil
			}
			rel = strings.TrimPrefix(strings.TrimPrefix(rel, subdir), "/")
			if rel == "" {
				return nil
			}
		}

		target := filepath.Join(destDir, rel)
		extracted++
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("failed to create parent directory: %w", err)
		}

		entry, err := sb.Open(p)
		if err != nil {
			return fmt.Errorf("failed to open entry: %w", err)
		}
		defer entry.Close()

		outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		if _, err := io.Copy(outFile, entry); err != nil {
			outFile.Close()
			return fmt.Errorf("failed to extract file: %w", err)
		}
		return outFile.Close()
	})
	if err != nil {
		return err
	}

	// See extractTar: an empty result from strip/subdir filtering is a
	// misconfiguration, not a success.
	if extracted == 0 && (strip || subdir != "") {
		return fmt.Errorf("no entries extracted (strip=%t, subdir=%q): archive layout does not match", strip, subdir)
	}
	return nil
}

// singleFileFS exposes exactly one file from an underlying fs.FS at its
// basename, so AddFS streams it (Open delegates to the real file) instead of
// buffering the whole thing. fsys is typically os.DirFS(parentDir).
type singleFileFS struct {
	fsys fs.FS
	name string
}

func (s singleFileFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return s.fsys.Open(".") // the (streaming) root directory handle
	case s.name:
		return s.fsys.Open(name)
	default:
		return nil, fs.ErrNotExist
	}
}

func (s singleFileFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	info, err := fs.Stat(s.fsys, s.name)
	if err != nil {
		return nil, err
	}
	return []fs.DirEntry{fs.FileInfoToDirEntry(info)}, nil
}
