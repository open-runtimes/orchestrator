package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStat_Interface(t *testing.T) {
	a := &Stat{ID: "s1", In: "result.bin", Depends: "process"}
	if a.ArtifactID() != "s1" {
		t.Errorf("ArtifactID() = %v, want s1", a.ArtifactID())
	}
	if a.ArtifactType() != "stat" {
		t.Errorf("ArtifactType() = %v, want stat", a.ArtifactType())
	}
	if a.DependsOn() != "process" {
		t.Errorf("DependsOn() = %v, want process", a.DependsOn())
	}
}

func TestStat_Apply(t *testing.T) {
	tmpDir := t.TempDir()
	content := []byte("hello world")
	os.WriteFile(filepath.Join(tmpDir, "result.bin"), content, 0o644)

	a := &Stat{ID: "test-stat", In: "result.bin"}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	size, ok := result.Content.(int64)
	if !ok {
		t.Fatalf("Expected int64 content, got %T", result.Content)
	}
	if size != int64(len(content)) {
		t.Errorf("Expected size %d, got %d", len(content), size)
	}
}

func TestStat_Apply_Missing(t *testing.T) {
	a := &Stat{ID: "test-stat", In: "nope.bin"}

	result := a.Apply(t.Context(), t.TempDir())
	if result.Error == nil {
		t.Fatal("Expected error for missing file")
	}
	if result.Status != "failed" {
		t.Errorf("Expected status 'failed', got %q", result.Status)
	}
}

func TestStat_Apply_Directory(t *testing.T) {
	a := &Stat{ID: "test-stat", In: "."}

	result := a.Apply(t.Context(), t.TempDir())
	if result.Error == nil {
		t.Fatal("Expected error for directory")
	}
	if result.Status != "failed" {
		t.Errorf("Expected status 'failed', got %q", result.Status)
	}
}
