package artifact

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	result := a.Apply(t.Context(), tmpDir)
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

	result := a.Apply(t.Context(), tmpDir)
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

	result := a.Apply(t.Context(), tmpDir)
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
	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
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
		ID:  "dl",
		In:  "http://127.0.0.1:1/source.tar.gz?X-Amz-Signature=deadbeefsecret&token=hunter2",
		Out: "out.tar.gz",
	}
	result := a.Apply(t.Context(), t.TempDir())
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
