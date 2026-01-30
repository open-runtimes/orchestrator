package types

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnarchive_Interface(t *testing.T) {
	a := &Unarchive{ID: "ua1", In: "src.tar.gz", Out: "src", Subdir: "functions/node"}
	if a.ArtifactID() != "ua1" {
		t.Errorf("ArtifactID() = %v, want ua1", a.ArtifactID())
	}
	if a.ArtifactType() != "unarchive" {
		t.Errorf("ArtifactType() = %v, want unarchive", a.ArtifactType())
	}
	if a.Subdir != "functions/node" {
		t.Errorf("Subdir = %v, want functions/node", a.Subdir)
	}
}

func TestUnarchive_Apply(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "unarchive-test")
	defer os.RemoveAll(tmpDir)

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"file1.txt":        "content1",
		"subdir/file2.txt": "content2",
	})

	a := &Unarchive{
		ID:  "test-unarchive",
		In:  "test.tar.gz",
		Out: "extracted",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	content1, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "file1.txt"))
	if err != nil {
		t.Fatalf("Failed to read file1.txt: %v", err)
	}
	if string(content1) != "content1" {
		t.Errorf("Expected 'content1', got %q", string(content1))
	}
}

func TestUnarchive_Apply_Subdir(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "unarchive-test")
	defer os.RemoveAll(tmpDir)

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"repo-main/README.md":                   "readme",
		"repo-main/functions/node/index.js":     "node code",
		"repo-main/functions/node/package.json": "{}",
		"repo-main/functions/python/main.py":    "python code",
	})

	a := &Unarchive{
		ID:     "test-unarchive-subdir",
		In:     "test.tar.gz",
		Out:    "extracted",
		Subdir: "functions/node",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	extractedDir := filepath.Join(tmpDir, "extracted")

	content, err := os.ReadFile(filepath.Join(extractedDir, "index.js"))
	if err != nil {
		t.Fatalf("Failed to read index.js: %v", err)
	}
	if string(content) != "node code" {
		t.Errorf("Expected 'node code', got %q", string(content))
	}

	if _, err := os.Stat(filepath.Join(extractedDir, "main.py")); !os.IsNotExist(err) {
		t.Error("main.py should not exist in extracted directory")
	}
}

func TestUnarchive_Apply_InTraversal(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "unarchive-test")
	defer os.RemoveAll(tmpDir)

	archiveIn := filepath.Join(tmpDir, "malicious.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"../../../etc/passwd": "malicious content",
	})

	a := &Unarchive{
		ID:  "test-unarchive",
		In:  "malicious.tar.gz",
		Out: "extracted",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error == nil {
		t.Error("Expected error for path traversal attempt")
	}
}

func createTestArchive(t *testing.T, archiveIn string, files map[string]string) {
	t.Helper()

	file, err := os.Create(archiveIn)
	if err != nil {
		t.Fatalf("Failed to create archive file: %v", err)
	}
	defer file.Close()

	gzWriter := gzip.NewWriter(file)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	for name, content := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("Failed to write tar header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("Failed to write tar content: %v", err)
		}
	}
}
