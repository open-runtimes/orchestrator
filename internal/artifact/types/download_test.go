package types

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	tmpDir, err := os.MkdirTemp("", "download-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	a := NewDownload(&http.Client{Timeout: 30 * time.Second}, "test-download", server.URL, "subdir/downloaded.txt", "")

	result := a.Apply(context.Background(), tmpDir)
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

	tmpDir, _ := os.MkdirTemp("", "download-test")
	defer os.RemoveAll(tmpDir)

	a := NewDownload(&http.Client{Timeout: 30 * time.Second}, "test-download", server.URL, "downloaded.txt", "")

	result := a.Apply(context.Background(), tmpDir)
	if result.Error == nil {
		t.Error("Expected error for 404 response")
	}
	if result.Status != "failed" {
		t.Errorf("Expected status 'failed', got %q", result.Status)
	}
}

func TestDownload_Apply_DefaultClient(t *testing.T) {
	expectedContent := "test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(expectedContent))
	}))
	defer server.Close()

	tmpDir, _ := os.MkdirTemp("", "download-test")
	defer os.RemoveAll(tmpDir)

	// No HTTP client set - should use default
	a := &Download{ID: "dl1", In: server.URL, Out: "file.txt"}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
}
