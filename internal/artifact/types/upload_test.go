package types

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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

	tmpDir, _ := os.MkdirTemp("", "upload-test")
	defer os.RemoveAll(tmpDir)

	testContent := "upload test content"
	os.WriteFile(filepath.Join(tmpDir, "output.txt"), []byte(testContent), 0o644)

	a := NewUpload(&http.Client{Timeout: 30 * time.Second}, 3, "test-upload", "output.txt", server.URL, "")

	result := a.Apply(context.Background(), tmpDir)
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
