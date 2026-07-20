package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// cleanSubdir normalizes a subdir filter to the slash-separated relative form
// archive entries use — "./astro/starter/" → "astro/starter" — so cosmetic
// prefixes don't miss every entry.
func cleanSubdir(s string) string {
	s = strings.Trim(path.Clean(s), "/")
	if s == "." {
		return ""
	}
	return s
}

// isGzip reports whether b starts with the gzip signature.
func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

// isZstd reports whether b starts with the zstd signature.
func isZstd(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

// isLz4 reports whether b starts with the lz4 frame signature.
func isLz4(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x04 && b[1] == 0x22 && b[2] == 0x4d && b[3] == 0x18
}

// isTar reports whether b holds the "ustar" signature at offset 257, which
// marks an uncompressed tar archive.
func isTar(b []byte) bool {
	return len(b) >= 262 && string(b[257:262]) == "ustar"
}

// Unarchive extracts a tar archive (plain, gzip, zstd, or lz4) or a squashfs or
// erofs image; the format is detected from the archive's magic bytes. (To mount
// a squashfs or erofs image read-only in place instead of materializing its
// files, use a "mount" artifact.)
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

// Apply extracts the archive, detecting squashfs / plain tar / gzip / zstd / lz4 from
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

	// Read enough to cover every magic we sniff. squashfs "hsqs" sits at offset
	// 0 and tar "ustar" at 257, but erofs's magic is at offset 1024 — so read
	// past it (1028 bytes) rather than the 512 the tar/squashfs checks needed.
	header := make([]byte, 1028)
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
		slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "strip", a.Strip, "format", "squashfs")
		return &Result{Status: "success"}
	case isErofs(header):
		if err := extractErofs(srcPath, destDir, a.Subdir, a.Strip); err != nil {
			return &Result{Status: "failed", Error: err}
		}
		slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "strip", a.Strip, "format", "erofs")
		return &Result{Status: "success"}
	case isGzip(header):
		return a.extractTar(srcPath, destDir, "gzip")
	case isZstd(header):
		return a.extractTar(srcPath, destDir, "zstd")
	case isLz4(header):
		return a.extractTar(srcPath, destDir, "lz4")
	case isTar(header):
		return a.extractTar(srcPath, destDir, "")
	default:
		return &Result{Status: "failed", Error: fmt.Errorf("unrecognized archive format for %s", a.In)}
	}
}

// extractTar extracts a tar archive at srcPath into destDir, decompressing the
// stream first with the named codec ("gzip", "zstd", "lz4", or "" for plain tar).
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
	case "lz4":
		src = lz4.NewReader(file)
	}

	tarReader := tar.NewReader(src)

	subdir := cleanSubdir(a.Subdir)
	var archiveRoot string

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

	slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "strip", a.Strip)
	return &Result{Status: "success"}
}

// extractFS materializes the contents of an image filesystem (squashfs or
// erofs, both exposed as an fs.FS) into destDir. Strip drops each entry's first
// path component; Subdir extracts only entries under that prefix, with the
// prefix removed. Shared by extractSquashfs and extractErofs.
func extractFS(fsys fs.FS, destDir, subdir string, strip bool) error {
	subdir = cleanSubdir(subdir)

	extracted := 0
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
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

		// Materialize only directories and regular files, matching extractTar.
		// Symlinks, FIFOs, devices, and sockets in an image are skipped rather
		// than mis-written as regular files (or failing the io.Copy below).
		if !d.IsDir() && !d.Type().IsRegular() {
			slog.Debug("Skipping non-regular archive entry", "name", p, "type", d.Type())
			return nil
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

		entry, err := fsys.Open(p)
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
