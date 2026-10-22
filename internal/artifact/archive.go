package artifact

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/pierrec/lz4/v4"
)

// Archive packs a file or directory into a tar, squashfs, or erofs archive.
//
// Format selects the container; Compression selects the algorithm. For tar an
// empty value means no compression; squashfs is always compressed (defaults to
// gzip). Level (1-9) sets the gzip level and applies to tar only. erofs images
// are always uncompressed and take no Compression, Level, or BlockSize.
type Archive struct {
	ID          string `json:"id"`
	In          string `json:"in"`                    // Source file or directory
	Out         string `json:"out"`                   // Destination archive path
	Format      string `json:"format"`                // "tar", "squashfs", or "erofs"
	Compression string `json:"compression,omitempty"` // tar: none, gzip, zstd, lz4, lz4hc; squashfs: gzip, zstd, lz4, lz4hc; erofs: none, lz4, lz4hc
	Level       int    `json:"level,omitempty"`       // gzip compression level 1-9 (tar only)
	BlockSize   int    `json:"blockSize,omitempty"`   // squashfs block size in bytes, power of 2 from 4 KiB to 1 MiB (default 1 MiB)
	Depends     string `json:"depends,omitempty"`
}

// squashfs block sizes are powers of two from 4 KiB to 1 MiB. The default
// matches mksquashfs -b 1M, the format open-runtimes/edge squashes use, and
// gives the best compression for these read-mostly mount images.
const (
	squashfsMinBlockSize     = 4 << 10
	squashfsMaxBlockSize     = 1 << 20
	defaultSquashfsBlockSize = 1 << 20
)

// validSquashfsBlockSize reports whether bs is a legal squashfs block size.
func validSquashfsBlockSize(bs int) bool {
	return bs >= squashfsMinBlockSize && bs <= squashfsMaxBlockSize && bs&(bs-1) == 0
}

// resolveSquashfsBlockSize maps an unset (0) block size to the 1 MiB default and
// rejects anything else that isn't a legal squashfs block size. The check runs
// before the uint32 conversion on purpose: a negative bs would otherwise wrap to
// a huge value that panics the writer's block splitting or yields a superblock
// BlockLog readers and kernel mounts reject. Callers may skip Validate, so this
// guards the write path itself rather than trusting the input.
func resolveSquashfsBlockSize(bs int) (uint32, error) {
	if bs == 0 {
		return defaultSquashfsBlockSize, nil
	}
	if !validSquashfsBlockSize(bs) {
		return 0, fmt.Errorf("invalid squashfs block size %d: must be a power of 2 between %d and %d", bs, squashfsMinBlockSize, squashfsMaxBlockSize)
	}
	return uint32(bs), nil
}

func (a *Archive) ArtifactID() string   { return a.ID }
func (a *Archive) ArtifactType() string { return "archive" }
func (a *Archive) DependsOn() string    { return a.Depends }

// sourceFS returns an fs.FS over the workspace entry name for streaming into an
// image writer (squashfs or erofs). A directory maps to its whole tree; a single
// file maps to a one-entry fs.FS (via singleFileFS) so the writer streams it
// rather than buffering the entire file in memory. Both are views of the
// workspace root, so nothing the tree links to can lead the writer out of it.
func sourceFS(root *os.Root, name string) (fs.FS, error) {
	// Image archives preserve the historical command-line behavior of
	// following a top-level directory symlink and packing the target tree. Tar
	// handles links separately below because it can represent the link itself.
	info, err := root.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("failed to stat source: %w", err)
	}
	if info.IsDir() {
		return fs.Sub(root.FS(), fsName(name))
	}
	parent, err := fs.Sub(root.FS(), fsName(filepath.Dir(name)))
	if err != nil {
		return nil, fmt.Errorf("failed to open source directory: %w", err)
	}
	return singleFileFS{fsys: parent, name: filepath.Base(name)}, nil
}

// gzipLevel maps an unset level (0) to the gzip default; otherwise returns it.
func gzipLevel(level int) int {
	if level == 0 {
		return pgzip.DefaultCompression
	}
	return level
}

// Apply creates the archive in the configured format.
func (a *Archive) Apply(ctx context.Context, basePath string) *Result {
	root, err := openWorkspace(basePath)
	if err != nil {
		return &Result{Status: "failed", Error: err}
	}
	defer root.Close()

	switch a.Format {
	case "tar":
		return a.applyTar(root)
	case "squashfs":
		if err := writeSquashfs(root, a.In, a.Out, a.Compression, a.BlockSize); err != nil {
			return &Result{Status: "failed", Error: err}
		}
		return &Result{Status: "success"}
	case "erofs":
		if err := writeErofs(root, a.In, a.Out, a.Compression); err != nil {
			return &Result{Status: "failed", Error: err}
		}
		return &Result{Status: "success"}
	default:
		return &Result{Status: "failed", Error: fmt.Errorf("%w: unsupported archive format: %s (supported: tar, squashfs, erofs)", ErrArchiveUnknownFormat, a.Format)}
	}
}

// applyTar creates a tar archive of In at Out, optionally compressed.
func (a *Archive) applyTar(root *os.Root) *Result {
	outFile, err := root.Create(a.Out)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create archive file: %w", err)}
	}
	defer outFile.Close()

	var w io.Writer = outFile
	switch a.Compression {
	case "", "none":
		// plain tar, no compression
	case "gzip":
		gzWriter, err := pgzip.NewWriterLevel(outFile, gzipLevel(a.Level))
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to create gzip writer: %w", err)}
		}
		defer gzWriter.Close()
		w = gzWriter
	case "zstd":
		zstdWriter, err := zstd.NewWriter(outFile)
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to create zstd writer: %w", err)}
		}
		defer zstdWriter.Close()
		w = zstdWriter
	case "lz4":
		lz4Writer := lz4.NewWriter(outFile)
		defer lz4Writer.Close()
		w = lz4Writer
	case "lz4hc":
		// Same lz4 frame format, high-compression encoder: better ratio for
		// more compress-time CPU, identical decompression.
		lz4Writer := lz4.NewWriter(outFile)
		if err := lz4Writer.Apply(lz4.CompressionLevelOption(lz4.Level9)); err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to configure lz4hc writer: %w", err)}
		}
		defer lz4Writer.Close()
		w = lz4Writer
	default:
		return &Result{Status: "failed", Error: fmt.Errorf("%w: unsupported tar compression: %q (supported: gzip, zstd, lz4, lz4hc, none)", ErrArchiveCompressionUnsupported, a.Compression)}
	}

	tarWriter := tar.NewWriter(w)
	defer tarWriter.Close()

	info, err := root.Lstat(a.In)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to stat source: %w", err)}
	}

	if info.IsDir() {
		src, err := fs.Sub(root.FS(), fsName(a.In))
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to open source: %w", err)}
		}
		if err := archiveDir(tarWriter, src); err != nil {
			return &Result{Status: "failed", Error: err}
		}
	} else {
		if err := archiveFile(tarWriter, root, a.In, info); err != nil {
			return &Result{Status: "failed", Error: err}
		}
	}

	return &Result{Status: "success"}
}

// archiveDir writes every entry of src into tw. src is a view of the
// workspace root: a symlink is recorded as a link, never followed, and a
// regular file can only be opened if it lives inside the workspace.
func archiveDir(tw *tar.Writer, src fs.FS) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("failed to stat entry: %w", err)
		}

		if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			linkname := ""
			if info.Mode()&os.ModeSymlink != 0 {
				linkname, err = fs.ReadLink(src, p)
				if err != nil {
					return fmt.Errorf("failed to read symlink: %w", err)
				}
			}
			header, err := tar.FileInfoHeader(info, linkname)
			if err != nil {
				return fmt.Errorf("failed to create tar header: %w", err)
			}
			header.Name = filepath.FromSlash(p)
			return tw.WriteHeader(header)
		}

		file, err := src.Open(p)
		if err != nil {
			return fmt.Errorf("failed to open file: %w", err)
		}
		defer file.Close()

		fileInfo, err := file.Stat()
		if err != nil {
			return fmt.Errorf("failed to stat file: %w", err)
		}

		header, err := tar.FileInfoHeader(fileInfo, "")
		if err != nil {
			return fmt.Errorf("failed to create tar header: %w", err)
		}
		header.Name = filepath.FromSlash(p)

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		if _, err := io.Copy(tw, file); err != nil {
			return fmt.Errorf("failed to write file to tar: %w", err)
		}

		return nil
	})
}

func archiveFile(tw *tar.Writer, root *os.Root, name string, info os.FileInfo) error {
	linkname := ""
	if info.Mode()&os.ModeSymlink != 0 {
		var err error
		linkname, err = root.Readlink(name)
		if err != nil {
			return fmt.Errorf("failed to read symlink: %w", err)
		}
	}
	header, err := tar.FileInfoHeader(info, linkname)
	if err != nil {
		return fmt.Errorf("failed to create tar header: %w", err)
	}
	header.Name = filepath.Base(name)

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("failed to write tar header: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}

	file, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("failed to write file to tar: %w", err)
	}

	return nil
}
