package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/internal/backoff"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	defaultUploadTimeoutSeconds = 300 // 5 minutes
	defaultUploadRetries        = 3
	dirUploadConcurrency        = 8
)

// Upload uploads a file to a URL. An Out ending in "/" uploads the directory
// In instead: each regular file under it goes to Out plus its path.
type Upload struct {
	ID             string            `json:"id"`
	In             string            `json:"in"`  // Path to read from
	Out            string            `json:"out"` // URL to upload to
	Depends        string            `json:"depends,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // HTTP timeout in seconds (default 300)
	Retries        int               `json:"retries,omitempty"`        // Max retry attempts (default 3)
	Headers        map[string]string `json:"headers,omitempty"`
	Exclude        []string          `json:"exclude,omitempty"` // Directory uploads skip any entry whose name matches one of these globs

	signer Signer // injected by the runner for s3:// URLs
}

func (a *Upload) ArtifactID() string   { return a.ID }
func (a *Upload) ArtifactType() string { return "upload" }
func (a *Upload) DependsOn() string    { return a.Depends }

// SetSigner satisfies signable.
func (a *Upload) SetSigner(s Signer) { a.signer = s }

// Apply uploads a file to a URL with retry.
func (a *Upload) Apply(ctx context.Context, root *os.Root) *Result {
	if strings.HasSuffix(a.Out, "/") {
		return a.applyDir(ctx, root)
	}
	// A regular file: a FIFO or device the workload left at the path would
	// otherwise hang the upload or stream without end. doUpload reopens it the
	// same way for each retry.
	file, fileInfo, err := openRegular(root, a.In)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to open file: %w", err)}
	}
	file.Close()
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
			wait := backoff.Exponential(attempt, backoff.Config{})
			slog.Debug("Retrying upload", "attempt", attempt, "backoff", wait, "path", a.In)
			select {
			case <-ctx.Done():
				return &Result{Status: "failed", Error: ctx.Err()}
			case <-time.After(wait):
			}
		}

		lastErr = a.doUpload(ctx, client, root, size)
		if lastErr == nil {
			if attempt > 0 {
				slog.Info("Upload succeeded after retry", "attempt", attempt, "path", a.In)
			}
			return &Result{Status: "success"}
		}

		if isClientError(lastErr) {
			return &Result{Status: "failed", Error: lastErr}
		}

		slog.Warn("Upload failed", "attempt", attempt, "error", lastErr, "path", a.In)
	}

	return &Result{Status: "failed", Error: fmt.Errorf("upload failed after %d retries: %w", maxRetries, lastErr)}
}

// applyDir uploads every regular file under In, a few at a time. Symlinks are
// skipped, and so is any file or directory whose name matches Exclude.
func (a *Upload) applyDir(ctx context.Context, root *os.Root) *Result {
	dir := path.Clean(a.In)
	var files []string
	err := fs.WalkDir(workspaceFS{root}, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && a.excluded(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			files = append(files, strings.TrimPrefix(p, dir+"/"))
		}
		return nil
	})
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to walk directory: %w", err)}
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, dirUploadConcurrency)
	var wg sync.WaitGroup
	for _, rel := range files {
		file := *a
		file.In = path.Join(dir, rel)
		file.Out = a.Out + (&url.URL{Path: rel}).EscapedPath()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if r := file.Apply(ctx, root); r.Error != nil {
				cancel(fmt.Errorf("%s: %w", rel, r.Error))
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return &Result{Status: "failed", Error: err}
	}
	return &Result{Status: "success"}
}

func (a *Upload) excluded(name string) bool {
	for _, pattern := range a.Exclude {
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

func (a *Upload) doUpload(ctx context.Context, client *http.Client, root *os.Root, size int64) error {
	file, _, err := openRegular(root, a.In)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	req, err := buildRequest(ctx, http.MethodPut, a.Out, file, size, a.signer, client.Timeout)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	for key, value := range a.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", redactTransferError(err))
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
