package types

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
)

// Unarchive extracts a tar.gz archive.
type Unarchive struct {
	ID      string `json:"id"`
	In      string `json:"in"`               // Source archive path
	Out     string `json:"out"`              // Destination directory
	Subdir  string `json:"subdir,omitempty"` // Extract only this subdirectory
	Depends string `json:"depends,omitempty"`
}

func (a *Unarchive) ArtifactID() string   { return a.ID }
func (a *Unarchive) ArtifactType() string { return "unarchive" }
func (a *Unarchive) DependsOn() string    { return a.Depends }

// Apply extracts a tar.gz archive.
// If Subdir is specified, only files under that subdirectory are extracted,
// with the subdir prefix stripped. GitHub archives have a root folder (e.g., "repo-main/")
// which is automatically detected and stripped when subdir is used.
func (a *Unarchive) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)
	destDir := filepath.Join(basePath, a.Out)

	file, err := os.Open(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to open archive: %w", err)}
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create gzip reader: %w", err)}
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	subdir := a.Subdir
	var archiveRoot string
	if subdir != "" {
		subdir = strings.Trim(subdir, "/")
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to read tar header: %w", err)}
		}

		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") {
			return &Result{Status: "failed", Error: fmt.Errorf("invalid path in archive: %s", header.Name)}
		}

		extractPath := cleanName
		if subdir != "" {
			if archiveRoot == "" {
				parts := strings.SplitN(cleanName, "/", 2)
				if len(parts) > 0 {
					archiveRoot = parts[0]
				}
			}

			fullSubdir := archiveRoot + "/" + subdir

			if !strings.HasPrefix(cleanName, fullSubdir+"/") && cleanName != fullSubdir {
				continue
			}

			extractPath = strings.TrimPrefix(cleanName, fullSubdir)
			extractPath = strings.TrimPrefix(extractPath, "/")
			if extractPath == "" {
				continue
			}
		}

		targetPath := filepath.Join(destDir, extractPath)

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

	slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir)
	return &Result{Status: "success"}
}
