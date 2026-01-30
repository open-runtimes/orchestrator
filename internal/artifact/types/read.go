package types

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Read reads file contents for inclusion in a callback event.
type Read struct {
	ID      string `json:"id"`
	In      string `json:"in"` // Path to read from
	Depends string `json:"depends,omitempty"`
}

func (a *Read) ArtifactID() string   { return a.ID }
func (a *Read) ArtifactType() string { return "read" }
func (a *Read) DependsOn() string    { return a.Depends }

// Apply reads file contents for inclusion in a callback event.
func (a *Read) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)

	content, err := os.ReadFile(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to read file: %w", err)}
	}

	var jsonContent any
	if err := json.Unmarshal(content, &jsonContent); err == nil {
		return &Result{Status: "success", Content: jsonContent}
	}

	return &Result{Status: "success", Content: string(content)}
}
