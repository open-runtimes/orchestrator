package types

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_Interface(t *testing.T) {
	a := &Write{ID: "w1", In: "content here", Out: "config.json"}
	if a.ArtifactID() != "w1" {
		t.Errorf("ArtifactID() = %v, want w1", a.ArtifactID())
	}
	if a.ArtifactType() != "write" {
		t.Errorf("ArtifactType() = %v, want write", a.ArtifactType())
	}
}

func TestWrite_Apply(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "write-test")
	defer os.RemoveAll(tmpDir)

	expectedContent := `{"key": "value"}`
	a := &Write{
		ID:  "test-write",
		In:  expectedContent,
		Out: "config.json",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}
	if string(content) != expectedContent {
		t.Errorf("Expected %q, got %q", expectedContent, string(content))
	}
}

func TestWrite_Apply_CreatesDirectory(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "write-test")
	defer os.RemoveAll(tmpDir)

	a := &Write{
		ID:  "test-write",
		In:  "content",
		Out: "subdir/nested/file.txt",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "subdir", "nested", "file.txt")); os.IsNotExist(err) {
		t.Error("File was not created in nested directory")
	}
}
