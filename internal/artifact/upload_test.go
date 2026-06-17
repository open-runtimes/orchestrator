package artifact

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestUpload_Interface(t *testing.T) {
	a := &Upload{ID: "ul1", In: "output.txt", Out: "https://example.com/upload", Depends: JobDependency}
	if a.ArtifactID() != "ul1" {
		t.Errorf("ArtifactID() = %v, want ul1", a.ArtifactID())
	}
	if a.ArtifactType() != "upload" {
		t.Errorf("ArtifactType() = %v, want upload", a.ArtifactType())
	}
	if a.DependsOn() != JobDependency {
		t.Errorf("DependsOn() = %v, want %v", a.DependsOn(), JobDependency)
	}
}

func TestUpload_Apply(t *testing.T) {
	var uploadReceived atomic.Bool
	var uploadContent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			content := make([]byte, r.ContentLength)
			r.Body.Read(content)
			uploadContent = content
			uploadReceived.Store(true)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	tmpDir := t.TempDir()

	testContent := "upload test content"
	os.WriteFile(filepath.Join(tmpDir, "output.txt"), []byte(testContent), 0o644)

	a := &Upload{ID: "test-upload", In: "output.txt", Out: server.URL, Retries: 3}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	if !uploadReceived.Load() {
		t.Error("Upload was not received")
	}
	if string(uploadContent) != testContent {
		t.Errorf("Expected content %q, got %q", testContent, string(uploadContent))
	}
}

func TestUpload_Apply_CustomConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "out.txt"), []byte("data"), 0o644)

	a := &Upload{ID: "ul1", In: "out.txt", Out: server.URL, TimeoutSeconds: 30, Retries: 1}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
}

func TestUpload_Apply_CustomHeaders(t *testing.T) {
	var gotAuth, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "out.txt"), []byte("data"), 0o644)

	a := &Upload{ID: "ul1", In: "out.txt", Out: server.URL, Retries: 1, Headers: map[string]string{
		"Authorization": "Bearer token",
		"Content-Type":  "text/plain", // must not override the fixed octet-stream type
	}}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if gotAuth != "Bearer token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer token")
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", gotContentType)
	}
}
