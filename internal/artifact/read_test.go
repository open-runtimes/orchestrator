package artifact

import (
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

func TestRead_Apply_JSON(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "result.json"), []byte(`{"status": "ok"}`), 0o644)

	a := &Read{
		ID:     "test-read",
		In:     "result.json",
		Format: "json",
	}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	m, ok := result.Content.(map[string]any)
	if !ok {
		t.Fatalf("Expected map content, got %T", result.Content)
	}
	if m["status"] != "ok" {
		t.Errorf("Expected status 'ok', got %v", m["status"])
	}
}

// Without an explicit format the content is always a raw string, even when the
// file happens to parse as JSON — consumers must not have to guess the type.
func TestRead_Apply_DefaultIsString(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "result.json"), []byte(`{"status": "ok"}`), 0o644)

	a := &Read{
		ID: "test-read",
		In: "result.json",
	}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	content, ok := result.Content.(string)
	if !ok {
		t.Fatalf("Expected string content, got %T", result.Content)
	}
	if content != `{"status": "ok"}` {
		t.Errorf("Expected raw JSON string, got %q", content)
	}
}

func TestRead_Apply_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "result.json"), []byte(`not json`), 0o644)

	a := &Read{
		ID:     "test-read",
		In:     "result.json",
		Format: "json",
	}

	result := a.Apply(t.Context(), tmpDir)
	if result.Status != "failed" || result.Error == nil {
		t.Fatalf("Expected failure for invalid JSON, got status %q, error %v", result.Status, result.Error)
	}
}

func TestRead_Apply_PlainText(t *testing.T) {
	tmpDir := t.TempDir()

	os.WriteFile(filepath.Join(tmpDir, "result.txt"), []byte("plain text content"), 0o644)

	a := &Read{
		ID: "test-read",
		In: "result.txt",
	}

	result := a.Apply(t.Context(), tmpDir)
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
