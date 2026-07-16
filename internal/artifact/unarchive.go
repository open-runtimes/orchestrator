package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// isGzip reports whether b starts with the gzip signature.
func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

// isZstd reports whether b starts with the zstd signature.
func isZstd(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

// isTar reports whether b holds the "ustar" signature at offset 257, which
// marks an uncompressed tar archive.
func isTar(b []byte) bool {
	return len(b) >= 262 && string(b[257:262]) == "ustar"
}

// Unarchive extracts a tar archive (plain, gzip, or zstd) or a squashfs image;
// the format is detected from the archive's magic bytes. (To mount a squashfs
// read-only in place instead of materializing its files, use a "mount"
// artifact.)
type Unarchive struct {
	ID      string `json:"id"`
	In      string `json:"in"`               // Source archive path
	Out     string `json:"out"`              // Destination directory
	Subdir  string `json:"subdir,omitempty"` // Extract only this subdirectory
	Strip   bool   `json:"strip,omitempty"`  // Drop the archive's wrapper root directory
	Depends string `json:"depends,omitempty"`
}

func (a *Unarchive) ArtifactID() string   { return a.ID }
func (a *Unarchive) ArtifactType() string { return "unarchive" }
func (a *Unarchive) DependsOn() string    { return a.Depends }

// Apply extracts the archive, detecting squashfs / plain tar / gzip / zstd from
// its magic bytes. If Strip is set, the first path component of every entry
// is dropped — git-forge archives (GitHub's "{repo}-{ref}/", Gitea's
// "{repo}/") wrap the tree in a single root directory whose name the caller
// can't always predict. If Subdir is specified, only files under that
// subdirectory are extracted, with the subdir prefix stripped; with Strip
// the subdir is resolved against the unwrapped tree, without it (legacy) a
// tar's detected root folder is implicitly prepended to the subdir.
func (a *Unarchive) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)
	destDir := filepath.Join(basePath, a.Out)

	// Read enough to cover the tar "ustar" magic at offset 257.
	header := make([]byte, 512)
	if f, err := os.Open(srcPath); err == nil {
		n, _ := io.ReadFull(f, header)
		header = header[:n]
		f.Close()
	}

	switch {
	case isSquashfs(header):
		if err := extractSquashfs(srcPath, destDir, a.Subdir, a.Strip); err != nil {
			return &Result{Status: "failed", Error: err}
		}
		slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "format", "squashfs")
		return &Result{Status: "success"}
	case isGzip(header):
		return a.extractTar(srcPath, destDir, "gzip")
	case isZstd(header):
		return a.extractTar(srcPath, destDir, "zstd")
	case isTar(header):
		return a.extractTar(srcPath, destDir, "")
	default:
		return &Result{Status: "failed", Error: fmt.Errorf("unrecognized archive format for %s", a.In)}
	}
}

// extractTar extracts a tar archive at srcPath into destDir, decompressing the
// stream first with the named codec ("gzip", "zstd", or "" for plain tar).
func (a *Unarchive) extractTar(srcPath, destDir, compression string) *Result {
	file, err := os.Open(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to open archive: %w", err)}
	}
	defer file.Close()

	var src io.Reader = file
	switch compression {
	case "gzip":
		gzReader, err := gzip.NewReader(file)
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to create gzip reader: %w", err)}
		}
		defer gzReader.Close()
		src = gzReader
	case "zstd":
		zstdReader, err := zstd.NewReader(file)
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to create zstd reader: %w", err)}
		}
		defer zstdReader.Close()
		src = zstdReader
	}

	tarReader := tar.NewReader(src)

	subdir := a.Subdir
	var archiveRoot string
	if subdir != "" {
		subdir = strings.Trim(subdir, "/")
	}

	extracted := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to read tar header: %w", err)}
		}

		// GitHub git-archive tarballs open with a pax global header (typeflag 'g')
		// carrying the commit SHA. Go's tar reader surfaces it as a real entry —
		// unlike Python, which consumes it transparently — and its synthetic
		// "pax_global_header" name would otherwise be mistaken for the archive
		// root, so a subdir match against "pax_global_header/<subdir>" skips every
		// real file. Skip global/extended headers; Go already applies their
		// metadata to the following entry.
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}

		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") {
			return &Result{Status: "failed", Error: fmt.Errorf("invalid path in archive: %s", header.Name)}
		}

		extractPath := cleanName
		if a.Strip {
			parts := strings.SplitN(cleanName, "/", 2)
			if len(parts) < 2 {
				continue // the wrapper root directory entry itself
			}
			extractPath = parts[1]
		}

		if subdir != "" {
			prefix := subdir
			if !a.Strip {
				// Legacy: resolve subdir under the tar's detected root folder.
				if archiveRoot == "" {
					archiveRoot = strings.SplitN(cleanName, "/", 2)[0]
				}
				prefix = archiveRoot + "/" + subdir
			}

			if !strings.HasPrefix(extractPath, prefix+"/") && extractPath != prefix {
				continue
			}

			extractPath = strings.TrimPrefix(extractPath, prefix)
			extractPath = strings.TrimPrefix(extractPath, "/")
			if extractPath == "" {
				continue
			}
		}

		targetPath := filepath.Join(destDir, extractPath)
		extracted++

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, os.FileMode(header.Mode)); err != nil {
				return &Result{Status: "failed", Error: fmt.Errorf("failed to create directory: %w", err)}
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return &Result{Status: "failed", Error: fmt.Errorf("failed to create parent directory: %w", err)}
			}

			outFile, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return &Result{Status: "failed", Error: fmt.Errorf("failed to create file: %w", err)}
			}

			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return &Result{Status: "failed", Error: fmt.Errorf("failed to extract file: %w", err)}
			}
			outFile.Close()

		default:
			slog.Debug("Skipping archive entry", "name", header.Name, "type", header.Typeflag)
		}
	}

	// strip on a flat archive (or a subdir that matches nothing) would
	// otherwise succeed with an empty destination — a hard-to-trace "no
	// source code" failure at whatever consumes the output. Fail here, where
	// the cause is still visible.
	if extracted == 0 && (a.Strip || subdir != "") {
		return &Result{Status: "failed", Error: fmt.Errorf("no entries extracted from %s (strip=%t, subdir=%q): archive layout does not match", a.In, a.Strip, subdir)}
	}

	slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir)
	return &Result{Status: "success"}
}
