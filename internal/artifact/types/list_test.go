package types

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestList_Interface(t *testing.T) {
	recursive := false
	a := &List{ID: "l1", In: "src", Excludes: []string{"node_modules"}, Recursive: &recursive}
	if a.ArtifactID() != "l1" {
		t.Errorf("ArtifactID() = %v, want l1", a.ArtifactID())
	}
	if a.ArtifactType() != "list" {
		t.Errorf("ArtifactType() = %v, want list", a.ArtifactType())
	}
	if *a.Recursive != false {
		t.Errorf("Recursive = %v, want false", *a.Recursive)
	}
}

func TestList_Apply(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "list-test")
	defer os.RemoveAll(tmpDir)

	os.MkdirAll(filepath.Join(tmpDir, "src", "subdir"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "src", "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "src", "util.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "src", "subdir", "helper.go"), []byte("package subdir"), 0o644)

	a := &List{
		ID: "test-list",
		In: "src",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	files, ok := result.Content.([]string)
	if !ok {
		t.Fatalf("Expected []string content, got %T", result.Content)
	}

	if len(files) != 3 {
		t.Errorf("Expected 3 files, got %d: %v", len(files), files)
	}
}

func TestList_Apply_WithExcludes(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "list-test")
	defer os.RemoveAll(tmpDir)

	os.MkdirAll(filepath.Join(tmpDir, "project", "src"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "project", "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "project", "src", "index.ts"), []byte("export {}"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "project", "package.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "project", "node_modules", "pkg", "index.js"), []byte(""), 0o644)

	a := &List{
		ID:       "test-list",
		In:       "project",
		Excludes: []string{"node_modules"},
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	files, ok := result.Content.([]string)
	if !ok {
		t.Fatalf("Expected []string content, got %T", result.Content)
	}

	if len(files) != 2 {
		t.Errorf("Expected 2 files, got %d: %v", len(files), files)
	}
}

func TestList_Apply_NonRecursive(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "list-test")
	defer os.RemoveAll(tmpDir)

	os.MkdirAll(filepath.Join(tmpDir, "project", "subdir"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "project", "root.txt"), []byte("root"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "project", "subdir", "nested.txt"), []byte("nested"), 0o644)

	recursive := false
	a := &List{
		ID:        "test-list",
		In:        "project",
		Recursive: &recursive,
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	files, ok := result.Content.([]string)
	if !ok {
		t.Fatalf("Expected []string content, got %T", result.Content)
	}

	if len(files) != 1 {
		t.Errorf("Expected 1 file (non-recursive), got %d: %v", len(files), files)
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	tests := []struct {
		path     string
		patterns []string
		want     bool
	}{
		{"file.txt", []string{"*.txt"}, true},
		{"file.go", []string{"*.txt"}, false},
		{"node_modules/pkg/index.js", []string{"node_modules"}, true},
		{"src/index.js", []string{"node_modules"}, false},
	}

	for _, tt := range tests {
		got := matchesAnyPattern(tt.path, tt.patterns)
		if got != tt.want {
			t.Errorf("matchesAnyPattern(%q, %v) = %v, want %v", tt.path, tt.patterns, got, tt.want)
		}
	}
}
