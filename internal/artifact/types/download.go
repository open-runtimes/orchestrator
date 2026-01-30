package types

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// Download downloads a file from a URL.
type Download struct {
	ID      string `json:"id"`
	In      string `json:"in"`  // URL to download from
	Out     string `json:"out"` // Path to write to
	Depends string `json:"depends,omitempty"`

	httpClient *http.Client
}

// NewDownload creates a Download artifact with the given HTTP client.
func NewDownload(httpClient *http.Client, id, in, out, depends string) *Download {
	return &Download{
		ID:         id,
		In:         in,
		Out:        out,
		Depends:    depends,
		httpClient: httpClient,
	}
}

// SetHTTPClient sets the HTTP client for this artifact.
func (a *Download) SetHTTPClient(c *http.Client) {
	a.httpClient = c
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

	client := a.httpClient
	if client == nil {
		client = http.DefaultClient
	}

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
