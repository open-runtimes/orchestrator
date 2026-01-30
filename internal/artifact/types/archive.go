package types

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Archive creates a tar.gz archive.
type Archive struct {
	ID      string `json:"id"`
	In      string `json:"in"`     // Source file or directory
	Out     string `json:"out"`    // Destination archive path
	Format  string `json:"format"` // Archive format (must be "tar.gz")
	Depends string `json:"depends,omitempty"`
}

func (a *Archive) ArtifactID() string   { return a.ID }
func (a *Archive) ArtifactType() string { return "archive" }
func (a *Archive) DependsOn() string    { return a.Depends }

// Apply creates a tar.gz archive.
func (a *Archive) Apply(ctx context.Context, basePath string) *Result {
	if a.Format != "tar.gz" {
		return &Result{Status: "failed", Error: fmt.Errorf("unsupported archive format: %s (supported: tar.gz)", a.Format)}
	}

	srcPath := filepath.Join(basePath, a.In)
	destPath := filepath.Join(basePath, a.Out)

	outFile, err := os.Create(destPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create archive file: %w", err)}
	}
	defer outFile.Close()

	gzWriter := gzip.NewWriter(outFile)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	info, err := os.Stat(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to stat source: %w", err)}
	}

	if info.IsDir() {
		if err := archiveDir(tarWriter, srcPath); err != nil {
			return &Result{Status: "failed", Error: err}
		}
	} else {
		if err := archiveFile(tarWriter, srcPath, info); err != nil {
			return &Result{Status: "failed", Error: err}
		}
	}

	return &Result{Status: "success"}
}

func archiveDir(tw *tar.Writer, srcDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		if relPath == "." {
			return nil
		}

		if info.IsDir() {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return fmt.Errorf("failed to create tar header: %w", err)
			}
			header.Name = relPath
			return tw.WriteHeader(header)
		}

		file, err := os.Open(path)
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
		header.Name = relPath

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("failed to write tar header: %w", err)
		}

		if _, err := io.Copy(tw, file); err != nil {
			return fmt.Errorf("failed to write file to tar: %w", err)
		}

		return nil
	})
}

func archiveFile(tw *tar.Writer, filePath string, info os.FileInfo) error {
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("failed to create tar header: %w", err)
	}
	header.Name = filepath.Base(filePath)

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("failed to write tar header: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("failed to write file to tar: %w", err)
	}

	return nil
}
