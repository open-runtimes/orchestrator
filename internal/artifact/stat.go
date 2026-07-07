package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Stat reports the size in bytes of a file for inclusion in a callback event.
type Stat struct {
	ID      string `json:"id"`
	In      string `json:"in"` // Path to stat
	Depends string `json:"depends,omitempty"`
}

func (a *Stat) ArtifactID() string   { return a.ID }
func (a *Stat) ArtifactType() string { return "stat" }
func (a *Stat) DependsOn() string    { return a.Depends }

// Apply reports the file size in bytes.
func (a *Stat) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)

	info, err := os.Stat(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to stat file: %w", err)}
	}
	if info.IsDir() {
		return &Result{Status: "failed", Error: fmt.Errorf("path is a directory, not a file: %s", a.In)}
	}

	return &Result{Status: "success", Content: info.Size()}
}
