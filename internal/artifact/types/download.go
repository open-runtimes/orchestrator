package types

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const defaultDownloadTimeoutSeconds = 300 // 5 minutes

// Download downloads a file from a URL.
type Download struct {
	ID             string `json:"id"`
	In             string `json:"in"`             // URL to download from
	Out            string `json:"out"`            // Path to write to
	Depends        string `json:"depends,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"` // HTTP timeout in seconds (default 300)
}

func (a *Download) ArtifactID() string   { return a.ID }
func (a *Download) ArtifactType() string { return "download" }
func (a *Download) DependsOn() string    { return a.Depends }

// Apply downloads a file from a URL.
func (a *Download) Apply(ctx context.Context, basePath string) *Result {
	destPath := filepath.Join(basePath, a.Out)

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create directory: %w", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.In, http.NoBody)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create request: %w", err)}
	}

	timeoutSecs := a.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = defaultDownloadTimeoutSeconds
	}
	client := &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to download file: %w", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return &Result{Status: "failed", Error: fmt.Errorf("download failed with status %d", resp.StatusCode)}
	}

	file, err := os.Create(destPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create file: %w", err)}
	}
	defer file.Close()

	written, err := io.Copy(file, resp.Body)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to write file: %w", err)}
	}

	if err := file.Sync(); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to sync file: %w", err)}
	}

	slog.Debug("Downloaded file", "bytes", written, "path", destPath)
	return &Result{Status: "success"}
}
