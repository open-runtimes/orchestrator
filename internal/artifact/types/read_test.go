package types

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRead_Interface(t *testing.T) {
	a := &Read{ID: "r1", In: "result.json", Depends: "process"}
	if a.ArtifactID() != "r1" {
		t.Errorf("ArtifactID() = %v, want r1", a.ArtifactID())
	}
	if a.ArtifactType() != "read" {
		t.Errorf("ArtifactType() = %v, want read", a.ArtifactType())
	}
	if a.DependsOn() != "process" {
		t.Errorf("DependsOn() = %v, want process", a.DependsOn())
	}
}

func TestRead_Apply(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "read-test")
	defer os.RemoveAll(tmpDir)

	os.WriteFile(filepath.Join(tmpDir, "result.json"), []byte(`{"status": "ok"}`), 0o644)

	a := &Read{
		ID: "test-read",
		In: "result.json",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	if result.Content == nil {
		t.Error("Expected content to be set")
	}

	m, ok := result.Content.(map[string]any)
	if !ok {
		t.Fatalf("Expected map content, got %T", result.Content)
	}
	if m["status"] != "ok" {
		t.Errorf("Expected status 'ok', got %v", m["status"])
	}
}

func TestRead_Apply_PlainText(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "read-test")
	defer os.RemoveAll(tmpDir)

	os.WriteFile(filepath.Join(tmpDir, "result.txt"), []byte("plain text content"), 0o644)

	a := &Read{
		ID: "test-read",
		In: "result.txt",
	}

	result := a.Apply(context.Background(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	content, ok := result.Content.(string)
	if !ok {
		t.Fatalf("Expected string content, got %T", result.Content)
	}
	if content != "plain text content" {
		t.Errorf("Expected 'plain text content', got %q", content)
	}
}
