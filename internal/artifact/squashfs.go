package artifact

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/KarpelesLab/squashfs"
	"github.com/klauspost/compress/zstd"
)

// KarpelesLab gates its own zstd compressor behind a build tag. We register it
// directly instead (klauspost is already a dependency) so squashfs zstd works
// in every build — symmetric with tar, and no build tags to thread through.
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
}

// squashfsMagic is the 4-byte signature at the start of every squashfs image.
const squashfsMagic = "hsqs"

// isSquashfs reports whether b starts with the squashfs signature.
func isSquashfs(b []byte) bool {
	return len(b) >= 4 && string(b[:4]) == squashfsMagic
}

// squashfsCompression maps a compression name to a squashfs algorithm. An empty
// name defaults to gzip — squashfs is always compressed. zstd requires the
// binary to be built with the "zstd" tag.
func squashfsCompression(name string) (squashfs.Compression, error) {
	switch name {
	case "", "gzip":
		return squashfs.GZip, nil
	case "zstd":
		return squashfs.ZSTD, nil
	default:
		return 0, fmt.Errorf("unsupported squashfs compression: %q (supported: gzip, zstd)", name)
	}
}

// writeSquashfs builds a squashfs image at destPath from the file or directory
// at srcPath.
func writeSquashfs(srcPath, destPath, compression string) error {
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

	w, err := squashfs.NewWriter(out, squashfs.WithCompression(comp))
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

	subdir = strings.Trim(subdir, "/")

	return fs.WalkDir(sb, ".", func(p string, d fs.DirEntry, err error) error {
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
