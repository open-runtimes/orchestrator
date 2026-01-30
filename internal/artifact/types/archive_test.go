package types

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestArchive_Interface(t *testing.T) {
	a := &Archive{ID: "a1", In: "src", Out: "src.tar.gz", Format: "tar.gz"}
	if a.ArtifactID() != "a1" {
		t.Errorf("ArtifactID() = %v, want a1", a.ArtifactID())
	}
	if a.ArtifactType() != "archive" {
		t.Errorf("ArtifactType() = %v, want archive", a.ArtifactType())
	}
}

func TestArchive_Apply(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "archive-test")
	defer os.RemoveAll(tmpDir)

	srcDir := filepath.Join(tmpDir, "source")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("content"), 0o644)

	a := &Archive{
		ID:     "test-archive",
		In:     "source",
		Out:    "output.tar.gz",
		Format: "tar.gz",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "output.tar.gz")); os.IsNotExist(err) {
		t.Error("Archive file was not created")
	}
}

func TestArchive_Apply_InvalidFormat(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "archive-test")
	defer os.RemoveAll(tmpDir)

	a := &Archive{
		ID:     "test-archive",
		In:     "source",
		Out:    "output.zip",
		Format: "zip",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error == nil {
		t.Error("Expected error for unsupported format")
	}
}
