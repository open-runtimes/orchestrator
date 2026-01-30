package types

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/pkg/backoff"
	"os"
	"path/filepath"
	"time"
)

// Upload uploads a file to a URL.
type Upload struct {
	ID      string `json:"id"`
	In      string `json:"in"`  // Path to read from
	Out     string `json:"out"` // URL to upload to
	Depends string `json:"depends,omitempty"`

	httpClient *http.Client
	maxRetries int
}

// NewUpload creates an Upload artifact with the given HTTP client and retry config.
func NewUpload(httpClient *http.Client, maxRetries int, id, in, out, depends string) *Upload {
	if maxRetries < 0 {
		maxRetries = 3
	}
	return &Upload{
		ID:         id,
		In:         in,
		Out:        out,
		Depends:    depends,
		httpClient: httpClient,
		maxRetries: maxRetries,
	}
}

// SetHTTPClient sets the HTTP client for this artifact.
func (a *Upload) SetHTTPClient(c *http.Client, maxRetries int) {
	a.httpClient = c
	a.maxRetries = maxRetries
}

func (a *Upload) ArtifactID() string   { return a.ID }
func (a *Upload) ArtifactType() string { return "upload" }
func (a *Upload) DependsOn() string    { return a.Depends }

// Apply uploads a file to a URL with retry.
func (a *Upload) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)

	fileInfo, err := os.Stat(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("file not found: %w", err)}
	}
	size := fileInfo.Size()

	client := a.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	maxRetries := a.maxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return &Result{Status: "failed", Error: err}
		}

		if attempt > 0 {
			wait := backoff.Exponential(attempt, nil)
			slog.Debug("Retrying upload", "attempt", attempt, "backoff", wait, "path", srcPath)
			select {
			case <-ctx.Done():
				return &Result{Status: "failed", Error: ctx.Err()}
			case <-time.After(wait):
			}
		}

		lastErr = a.doUpload(ctx, client, srcPath, size)
		if lastErr == nil {
			if attempt > 0 {
				slog.Info("Upload succeeded after retry", "attempt", attempt, "path", srcPath)
			}
			return &Result{Status: "success"}
		}

		if isClientError(lastErr) {
			return &Result{Status: "failed", Error: lastErr}
		}

		slog.Warn("Upload failed", "attempt", attempt, "error", lastErr, "path", srcPath)
	}

	return &Result{Status: "failed", Error: fmt.Errorf("upload failed after %d retries: %w", maxRetries, lastErr)}
}

func (a *Upload) doUpload(ctx context.Context, client *http.Client, filePath string, size int64) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.Out, file)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		slog.Debug("Uploaded file", "bytes", size)
		return nil
	}

	respBody, _ := io.ReadAll(resp.Body)
	return &uploadError{statusCode: resp.StatusCode, message: string(respBody)}
}

type uploadError struct {
	statusCode int
	message    string
}

func (e *uploadError) Error() string {
	return fmt.Sprintf("upload failed with status %d: %s", e.statusCode, e.message)
}

func isClientError(err error) bool {
	if ue, ok := err.(*uploadError); ok {
		return ue.statusCode >= 400 && ue.statusCode < 500
	}
	return false
}
