package artifact

import (
	"fmt"
	"io"
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
			content, _ := io.ReadAll(r.Body)
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

func TestUpload_Apply_Chunked(t *testing.T) {
	var chunks atomic.Int64
	var uploaded int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		chunk := chunks.Add(1)
		wantRange := fmt.Sprintf("bytes %d-%d/%d", (chunk-1)*uploadChunkSize, min(chunk*uploadChunkSize, uploadChunkSize+1)-1, uploadChunkSize+1)
		if r.Header.Get("Content-Range") != wantRange {
			t.Errorf("Content-Range = %q, want %q", r.Header.Get("Content-Range"), wantRange)
		}

		content, _ := io.ReadAll(r.Body)
		uploaded += int64(len(content))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	payload := make([]byte, uploadChunkSize+1)
	if err := os.WriteFile(filepath.Join(tmpDir, "output.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Upload{ID: "test-upload", In: "output.bin", Out: server.URL, Retries: 1}
	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	if chunks.Load() != 2 {
		t.Errorf("chunks = %d, want 2", chunks.Load())
	}
	if uploaded != int64(len(payload)) {
		t.Errorf("uploaded = %d, want %d", uploaded, len(payload))
	}
}
