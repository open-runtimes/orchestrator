package artifact

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownload_Interface(t *testing.T) {
	a := &Download{ID: "dl1", In: "https://example.com/file", Out: "input.txt", Depends: "other"}
	if a.ArtifactID() != "dl1" {
		t.Errorf("ArtifactID() = %v, want dl1", a.ArtifactID())
	}
	if a.ArtifactType() != "download" {
		t.Errorf("ArtifactType() = %v, want download", a.ArtifactType())
	}
	if a.DependsOn() != "other" {
		t.Errorf("DependsOn() = %v, want other", a.DependsOn())
	}
}

func TestDownload_Apply(t *testing.T) {
	expectedContent := "test file content"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(expectedContent))
	}))
	defer server.Close()

	tmpDir := t.TempDir()

	a := &Download{ID: "test-download", In: server.URL, Out: "subdir/downloaded.txt"}

	result := a.Apply(t.Context(), workspace(t, tmpDir))
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if result.Status != "success" {
		t.Errorf("Expected status 'success', got %q", result.Status)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "subdir", "downloaded.txt"))
	if err != nil {
		t.Fatalf("Failed to read downloaded file: %v", err)
	}
	if string(content) != expectedContent {
		t.Errorf("Expected content %q, got %q", expectedContent, string(content))
	}
}

func TestDownload_Apply_Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	tmpDir := t.TempDir()

	a := &Download{ID: "test-download", In: server.URL, Out: "downloaded.txt"}

	result := a.Apply(t.Context(), workspace(t, tmpDir))
	if result.Error == nil {
		t.Error("Expected error for 404 response")
	}
	if result.Status != "failed" {
		t.Errorf("Expected status 'failed', got %q", result.Status)
	}
}

func TestDownload_Apply_CustomTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()

	a := &Download{ID: "dl1", In: server.URL, Out: "file.txt", TimeoutSeconds: 30}

	result := a.Apply(t.Context(), workspace(t, tmpDir))
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
}

func TestDownload_Apply_LeavesNoPartialFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("content"))
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	a := &Download{ID: "dl", In: server.URL, Out: "file.bin"}
	if result := a.Apply(t.Context(), workspace(t, tmpDir)); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "file.bin.partial")); err == nil {
		t.Fatal("partial file left behind after a successful download")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "file.bin")); err != nil {
		t.Fatalf("final file missing: %v", err)
	}
}

// A presigned URL carries its credential in the query string. A transport
// failure must not leak it into the artifact's error, which travels into the
// callback, the logs and the job status.
func TestDownload_Apply_RedactsURLCredentialsOnError(t *testing.T) {
	a := &Download{
		ID:             "dl",
		In:             "http://127.0.0.1:1/source.tar.gz?X-Amz-Signature=deadbeefsecret&token=hunter2",
		Out:            "out.tar.gz",
		TimeoutSeconds: 2, // a refused connection is retried until the budget runs out
	}
	result := a.Apply(t.Context(), workspace(t, t.TempDir()))
	if result.Error == nil {
		t.Fatal("expected a connection error")
	}
	msg := result.Error.Error()
	for _, secret := range []string{"deadbeefsecret", "hunter2", "X-Amz-Signature"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error leaked %q: %s", secret, msg)
		}
	}
	// The host still names what failed.
	if !strings.Contains(msg, "127.0.0.1") {
		t.Errorf("error should still name the host: %s", msg)
	}
}

// flaky answers status, with Retry-After when after is set, to the first
// failures requests and serves content after that. It counts every request.
func flaky(t *testing.T, failures, status int, after, content string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(requests.Add(1)) <= failures {
			if after != "" {
				w.Header().Set("Retry-After", after)
			}
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(content))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// A server that is briefly unavailable or rate limiting costs the download a
// retry, not the artifact.
func TestDownload_Apply_RetriesTransientStatus(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server, requests := flaky(t, 1, status, "", "variables")
			tmpDir := t.TempDir()

			a := &Download{ID: "env", In: server.URL, Out: "env/variables", TimeoutSeconds: 10}
			if result := a.Apply(t.Context(), workspace(t, tmpDir)); result.Error != nil {
				t.Fatalf("Apply() error = %v", result.Error)
			}
			if got := requests.Load(); got != 2 {
				t.Errorf("requests = %d, want 2", got)
			}
			if content, _ := os.ReadFile(filepath.Join(tmpDir, "env", "variables")); string(content) != "variables" {
				t.Errorf("content = %q, want %q", content, "variables")
			}
		})
	}
}

// A connection the server drops before answering is retried like a 5xx.
func TestDownload_Apply_RetriesDroppedConnection(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)

	a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 10}
	if result := a.Apply(t.Context(), workspace(t, t.TempDir())); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// A connection cut partway through the body is retried, and the partial body
// is not left behind.
func TestDownload_Apply_RetriesBodyCutShort(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
			buf.Flush()
			conn.Close()
			return
		}
		w.Write([]byte("complete"))
	}))
	t.Cleanup(server.Close)
	tmpDir := t.TempDir()

	a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 10}
	if result := a.Apply(t.Context(), workspace(t, tmpDir)); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
	if content, _ := os.ReadFile(filepath.Join(tmpDir, "file.txt")); string(content) != "complete" {
		t.Errorf("content = %q, want %q", content, "complete")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "file.txt.partial")); err == nil {
		t.Error("partial file left behind")
	}
}

// A caller that cancels during the wait sees a cancellation, not the last
// attempt's status.
func TestDownload_Apply_CancelDuringBackoffIsCanceled(t *testing.T) {
	t.Parallel()
	server, _ := flaky(t, 1<<30, http.StatusServiceUnavailable, "5", "")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)

	a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 30}
	result := a.Apply(ctx, workspace(t, t.TempDir()))
	if result.Error == nil {
		t.Fatal("expected the download to fail")
	}
	if code := FailureCode("download", result.Error); code != ErrArtifactCanceled {
		t.Errorf("FailureCode = %q, want %q (error %v)", code, ErrArtifactCanceled, result.Error)
	}
}

// A client error is the answer, not a hiccup: asking again would only delay
// the failure, and callers such as a delta restore branch on a 404.
func TestDownload_Apply_DoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			server, requests := flaky(t, 1, status, "1", "unreachable")

			a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 10}
			result := a.Apply(t.Context(), workspace(t, t.TempDir()))
			if result.Error == nil {
				t.Fatalf("expected a %d to fail the download", status)
			}
			if got := requests.Load(); got != 1 {
				t.Errorf("requests = %d, want 1", got)
			}
			var statusErr *HTTPStatusError
			if !errors.As(result.Error, &statusErr) || statusErr.StatusCode != status {
				t.Errorf("error = %v, want an HTTPStatusError with %d", result.Error, status)
			}
			if code := FailureCode("download", result.Error); code != ErrDownloadHTTPError {
				t.Errorf("FailureCode = %q, want %q", code, ErrDownloadHTTPError)
			}
		})
	}
}

// The next attempt waits for the Retry-After the server asked for.
func TestDownload_Apply_HonoursRetryAfter(t *testing.T) {
	t.Parallel()
	server, requests := flaky(t, 1, http.StatusServiceUnavailable, "1", "ok")

	a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 10}
	start := time.Now()
	if result := a.Apply(t.Context(), workspace(t, t.TempDir())); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("retried after %s, want at least the 1s Retry-After", took)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
}

// A Retry-After past the budget fails now rather than sleeping into the
// deadline.
func TestDownload_Apply_RetryAfterBeyondBudgetFailsAtOnce(t *testing.T) {
	t.Parallel()
	server, requests := flaky(t, 1, http.StatusServiceUnavailable, "60", "ok")

	a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 2}
	start := time.Now()
	if result := a.Apply(t.Context(), workspace(t, t.TempDir())); result.Error == nil {
		t.Fatal("expected the download to give up")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("gave up after %s, want at once", took)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// TimeoutSeconds bounds every attempt together, and the last status survives
// as the cause so the failure is still classified.
func TestDownload_Apply_GivesUpWithinTimeout(t *testing.T) {
	t.Parallel()
	server, requests := flaky(t, 1<<30, http.StatusServiceUnavailable, "", "")

	a := &Download{ID: "dl", In: server.URL, Out: "file.txt", TimeoutSeconds: 2}
	start := time.Now()
	result := a.Apply(t.Context(), workspace(t, t.TempDir()))
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("gave up after %s, want within the 2s timeout", took)
	}
	if result.Error == nil {
		t.Fatal("expected the download to fail")
	}
	if got := requests.Load(); got < 2 {
		t.Errorf("requests = %d, want retries before giving up", got)
	}
	var statusErr *HTTPStatusError
	if !errors.As(result.Error, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("error = %v, want an HTTPStatusError with 503", result.Error)
	}
	if code := FailureCode("download", result.Error); code != ErrDownloadHTTPError {
		t.Errorf("FailureCode = %q, want %q", code, ErrDownloadHTTPError)
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	tests := map[string]time.Duration{
		"":                              0,
		"3":                             3 * time.Second,
		"-1":                            0,
		"soon":                          0,
		"Mon, 01 Jan 2001 00:00:00 GMT": 0,
	}
	for value, want := range tests {
		if got := retryAfter(value); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", value, got, want)
		}
	}
	if got := retryAfter(time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)); got < 59*time.Minute {
		t.Errorf("retryAfter(an hour from now) = %s, want about an hour", got)
	}
}
