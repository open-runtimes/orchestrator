package artifact

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
)

// List lists files in a directory.
type List struct {
	ID          string   `json:"id"`
	In          string   `json:"in"`                  // Directory to list
	IsRecursive *bool    `json:"recursive,omitempty"` // Default: true
	Excludes    []string `json:"excludes,omitempty"`  // Glob patterns to exclude
	Depends     string   `json:"depends,omitempty"`
}

func (a *List) ArtifactID() string   { return a.ID }
func (a *List) ArtifactType() string { return "list" }
func (a *List) DependsOn() string    { return a.Depends }

// Apply lists files in a directory.
func (a *List) Apply(ctx context.Context, basePath string) *Result {
	root, err := openWorkspace(basePath)
	if err != nil {
		return &Result{Status: "failed", Error: err}
	}
	defer root.Close()

	recursive := a.IsRecursive == nil || *a.IsRecursive

	info, err := root.Stat(a.In)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to stat path: %w", err)}
	}

	if !info.IsDir() {
		baseName := filepath.Base(a.In)
		if !matchesAnyPattern(baseName, a.Excludes) {
			return &Result{Status: "success", Content: []string{baseName}}
		}
		return &Result{Status: "success", Content: []string{}}
	}

	files := []string{} // an empty listing serializes as [], never null

	dir, err := fs.Sub(root.FS(), fsName(a.In))
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to open directory: %w", err)}
	}

	if recursive {
		err = fs.WalkDir(dir, ".", a.walkCollect(&files))
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to walk directory: %w", err)}
		}
	} else {
		entries, err := fs.ReadDir(dir, ".")
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

	slog.Debug("Listed files", "path", filepath.Join(basePath, a.In), "count", len(files), "recursive", recursive)
	return &Result{Status: "success", Content: files}
}

func (a *List) walkCollect(files *[]string) fs.WalkDirFunc {
	return func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		relPath := filepath.FromSlash(p)
		if matchesAnyPattern(relPath, a.Excludes) || matchesAnyPattern(d.Name(), a.Excludes) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			*files = append(*files, relPath)
		}
		return nil
	}
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
