package artifact

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/internal/config"
	"os"
	"path/filepath"
	"time"
)

const defaultDownloadTimeoutSeconds = 300 // 5 minutes

// Download downloads a file from a URL.
type Download struct {
	ID             string            `json:"id"`
	In             string            `json:"in"`  // URL to download from
	Out            string            `json:"out"` // Path to write to
	Depends        string            `json:"depends,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // HTTP timeout in seconds (default 300)
	Headers        map[string]string `json:"headers,omitempty"`
	// SkipIfExists treats an existing target as this download, already
	// complete: writes land in a temp file and are renamed into place, so
	// presence implies integrity. Declared for immutable sources (a build
	// archive) so a restarted sidecar recovers in place instead of re-fetching
	// — or truncating — a file a running workload may be mounted on. Leave
	// unset for sources that change between fetches into a persistent
	// workspace, like a workspace delta.
	SkipIfExists bool `json:"skipIfExists,omitempty"`

	creds config.S3Credentials // injected by the runner for s3:// URLs
}

func (a *Download) ArtifactID() string   { return a.ID }
func (a *Download) ArtifactType() string { return "download" }
func (a *Download) DependsOn() string    { return a.Depends }

// SetS3Credentials satisfies S3Configurable.
func (a *Download) SetS3Credentials(c config.S3Credentials) { a.creds = c }

// Apply downloads a file from a URL.
func (a *Download) Apply(ctx context.Context, basePath string) *Result {
	destPath := filepath.Join(basePath, a.Out)

	if a.SkipIfExists {
		if _, err := os.Stat(destPath); err == nil {
			slog.Info("Download target already present, skipping", "path", destPath)
			return &Result{Status: "success"}
		}
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create directory: %w", err)}
	}

	req, err := buildRequest(ctx, http.MethodGet, a.In, nil, 0, a.creds)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create request: %w", err)}
	}
	applyHeaders(req, a.Headers)

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

	tmpPath := destPath + ".partial"
	file, err := os.Create(tmpPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create file: %w", err)}
	}
	defer func() {
		file.Close()
		os.Remove(tmpPath) // no-op after the rename; removes debris on failure
	}()

	written, err := io.Copy(file, resp.Body)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to write file: %w", err)}
	}

	if err := file.Sync(); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to sync file: %w", err)}
	}
	if err := file.Close(); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to close file: %w", err)}
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to move file into place: %w", err)}
	}

	slog.Debug("Downloaded file", "bytes", written, "path", destPath)
	return &Result{Status: "success"}
}
