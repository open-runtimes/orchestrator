package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/pkg/backoff"
	"os"
	"path/filepath"
	"time"
)

const (
	defaultUploadTimeoutSeconds = 300 // 5 minutes
	defaultUploadRetries        = 3
)

// Upload uploads a file to a URL.
type Upload struct {
	ID             string `json:"id"`
	In             string `json:"in"`  // Path to read from
	Out            string `json:"out"` // URL to upload to
	Depends        string `json:"depends,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"` // HTTP timeout in seconds (default 300)
	Retries        int    `json:"retries,omitempty"`        // Max retry attempts (default 3)
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

	timeoutSecs := a.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = defaultUploadTimeoutSeconds
	}
	client := &http.Client{Timeout: time.Duration(timeoutSecs) * time.Second}

	maxRetries := a.Retries
	if maxRetries <= 0 {
		maxRetries = defaultUploadRetries
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
	ue := &uploadError{}
	if errors.As(err, &ue) {
		return ue.statusCode >= 400 && ue.statusCode < 500
	}
	return false
}
