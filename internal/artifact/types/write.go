package types

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Write writes inline content to a file.
type Write struct {
	ID      string `json:"id"`
	In      string `json:"in"`  // Content to write
	Out     string `json:"out"` // Path to write to
	Depends string `json:"depends,omitempty"`
}

func (a *Write) ArtifactID() string   { return a.ID }
func (a *Write) ArtifactType() string { return "write" }
func (a *Write) DependsOn() string    { return a.Depends }

// Apply writes inline content to a file.
func (a *Write) Apply(ctx context.Context, basePath string) *Result {
	destPath := filepath.Join(basePath, a.Out)

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create directory: %w", err)}
	}

	if err := os.WriteFile(destPath, []byte(a.In), 0o644); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to write file: %w", err)}
	}

	slog.Debug("Wrote file", "bytes", len(a.In), "path", destPath)
	return &Result{Status: "success"}
}
