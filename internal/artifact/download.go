package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"orchestrator/internal/backoff"
)

const (
	defaultDownloadTimeoutSeconds = 300 // 5 minutes

	// downloadRetryInitial and downloadRetryMax space the attempts at a
	// transient failure. A Retry-After the server sends is the floor for the
	// next wait, never a reason to come back sooner.
	downloadRetryInitial = 500 * time.Millisecond
	downloadRetryMax     = 5 * time.Second
)

// Download downloads a file from a URL.
//
// A connection failure, a 5xx or a 429 is retried with exponential backoff
// until TimeoutSeconds, which bounds every attempt together rather than each
// one. Any other status fails at once: a 404 or 401 does not change by asking
// again, and callers branch on it.
type Download struct {
	ID             string            `json:"id"`
	In             string            `json:"in"`  // URL to download from
	Out            string            `json:"out"` // Path to write to
	Depends        string            `json:"depends,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // Budget for all attempts in seconds (default 300)
	Headers        map[string]string `json:"headers,omitempty"`

	signer Signer // injected by the runner for s3:// URLs
}

// HTTPStatusError is a download response outside 2xx. Callers that treat a
// missing object as "nothing there yet" branch on StatusCode.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("download failed with status %d", e.StatusCode)
}

// retryableError marks an attempt's failure as one a later attempt may not
// hit. after is the server's Retry-After, zero when it sent none.
type retryableError struct {
	err   error
	after time.Duration
}

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (a *Download) ArtifactID() string   { return a.ID }
func (a *Download) ArtifactType() string { return "download" }
func (a *Download) DependsOn() string    { return a.Depends }

// SetSigner satisfies signable.
func (a *Download) SetSigner(s Signer) { a.signer = s }

// Apply downloads a file from a URL, retrying transient failures within the
// artifact's timeout.
func (a *Download) Apply(ctx context.Context, root *os.Root) *Result {
	if err := root.MkdirAll(filepath.Dir(a.Out), 0o755); err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to create directory: %w", err)}
	}

	timeoutSecs := a.TimeoutSeconds
	if timeoutSecs <= 0 {
		timeoutSecs = defaultDownloadTimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
	defer cancel()
	// The context carries the budget, body included, so it spans retries.
	client := &http.Client{}

	for attempt := 1; ; attempt++ {
		err := a.attempt(ctx, client, root)
		if err == nil {
			if attempt > 1 {
				slog.Info("Download succeeded after retry", "artifactId", a.ID, "attempt", attempt)
			}
			return &Result{Status: "success"}
		}

		var retryable *retryableError
		if !errors.As(err, &retryable) {
			return &Result{Status: "failed", Error: err}
		}
		wait := max(backoff.Exponential(attempt, backoff.Config{Initial: downloadRetryInitial, Max: downloadRetryMax}), retryable.after)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
			return &Result{Status: "failed", Error: gaveUp(retryable.err, attempt)}
		}
		slog.Warn("Download failed, retrying", "artifactId", a.ID, "attempt", attempt, "backoff", wait, "error", retryable.err)
		select {
		case <-ctx.Done():
			return &Result{Status: "failed", Error: gaveUp(retryable.err, attempt)}
		case <-time.After(wait):
		}
	}
}

// gaveUp keeps the last attempt's error, and its classification, as the cause.
func gaveUp(err error, attempts int) error {
	if attempts == 1 {
		return err
	}
	return fmt.Errorf("%w (gave up after %d attempts)", err, attempts)
}

// attempt makes one request and, on a 200, moves the body into place. Only
// failures another attempt might not hit come back as *retryableError.
func (a *Download) attempt(ctx context.Context, client *http.Client, root *os.Root) error {
	timeout := time.Duration(defaultDownloadTimeoutSeconds) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	req, err := buildRequest(ctx, http.MethodGet, a.In, nil, 0, a.signer, timeout)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	for key, value := range a.Headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("failed to download file: %w", redactTransferError(err))
		if ctx.Err() == nil && isConnectionError(err) {
			return &retryableError{err: err}
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("%w: %w", ErrDownloadHTTPError, &HTTPStatusError{StatusCode: resp.StatusCode})
		if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // lets the connection be reused
			return &retryableError{err: err, after: retryAfter(resp.Header.Get("Retry-After"))}
		}
		return err
	}

	tmpName := a.Out + ".partial"
	file, err := root.Create(tmpName)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() {
		file.Close()
		_ = root.Remove(tmpName) // no-op after the rename; removes debris on failure
	}()

	body := &bodyReader{r: resp.Body}
	written, err := io.Copy(file, body)
	if err != nil {
		err = fmt.Errorf("failed to write file: %w", redactTransferError(err))
		// A connection cut mid-body is worth another attempt; a full disk is not.
		if body.err != nil && ctx.Err() == nil && isConnectionError(body.err) {
			return &retryableError{err: err}
		}
		return err
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}

	if err := root.Rename(tmpName, a.Out); err != nil {
		return fmt.Errorf("failed to move file into place: %w", err)
	}

	slog.Debug("Downloaded file", "bytes", written, "path", filepath.Join(root.Name(), a.Out))
	return nil
}

// bodyReader remembers a read error, so a failed copy can tell the
// connection's fault from the disk's.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// isConnectionError reports a failure of the connection itself: refused,
// reset, unresolvable, or closed before a full response. A malformed URL, an
// unsupported scheme or a rejected certificate is not one, and fails at once.
func isConnectionError(err error) bool {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &opErr) || errors.As(err, &dnsErr) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryAfter parses a Retry-After header, in seconds or as an HTTP date. An
// absent, malformed or past value is zero.
func retryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return max(time.Duration(seconds)*time.Second, 0)
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}
