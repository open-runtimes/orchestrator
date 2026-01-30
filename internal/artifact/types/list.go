package types

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// List lists files in a directory.
type List struct {
	ID        string   `json:"id"`
	In        string   `json:"in"`                  // Directory to list
	Recursive *bool    `json:"recursive,omitempty"` // Default: true
	Excludes  []string `json:"excludes,omitempty"`  // Glob patterns to exclude
	Depends   string   `json:"depends,omitempty"`
}

func (a *List) ArtifactID() string   { return a.ID }
func (a *List) ArtifactType() string { return "list" }
func (a *List) DependsOn() string    { return a.Depends }

// Apply lists files in a directory.
func (a *List) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)
	recursive := a.Recursive == nil || *a.Recursive

	info, err := os.Stat(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to stat path: %w", err)}
	}

	if !info.IsDir() {
		baseName := filepath.Base(srcPath)
		if !matchesAnyPattern(baseName, a.Excludes) {
			return &Result{Status: "success", Content: []string{baseName}}
		}
		return &Result{Status: "success", Content: []string{}}
	}

	var files []string

	if recursive {
		err = filepath.Walk(srcPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			relPath, err := filepath.Rel(srcPath, path)
			if err != nil {
				return err
			}

			if relPath == "." {
				return nil
			}

			if matchesAnyPattern(relPath, a.Excludes) || matchesAnyPattern(filepath.Base(path), a.Excludes) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			if !info.IsDir() {
				files = append(files, relPath)
			}

			return nil
		})
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to walk directory: %w", err)}
		}
	} else {
		entries, err := os.ReadDir(srcPath)
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to read directory: %w", err)}
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			name := entry.Name()
			if !matchesAnyPattern(name, a.Excludes) {
				files = append(files, name)
			}
		}
	}

	slog.Debug("Listed files", "path", srcPath, "count", len(files), "recursive", recursive)
	return &Result{Status: "success", Content: files}
}

func matchesAnyPattern(path string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := filepath.Match(pattern, path); matched {
			return true
		}
		if matched, _ := filepath.Match(pattern, filepath.Base(path)); matched {
			return true
		}
		if strings.HasPrefix(path, pattern+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
