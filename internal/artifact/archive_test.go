package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArchive_Interface(t *testing.T) {
	a := &Archive{ID: "a1", In: "src", Out: "src.tar.gz", Format: "tar"}
	if a.ArtifactID() != "a1" {
		t.Errorf("ArtifactID() = %v, want a1", a.ArtifactID())
	}
	if a.ArtifactType() != "archive" {
		t.Errorf("ArtifactType() = %v, want archive", a.ArtifactType())
	}
}

func TestArchive_Apply(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "source")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("content"), 0o644)

	a := &Archive{
		ID:     "test-archive",
		In:     "source",
		Out:    "output.tar.gz",
		Format: "tar",
	}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}
	if result.Format != "tar" || result.Compression != "none" {
		t.Errorf("classification = %s/%s, want tar/none", result.Format, result.Compression)
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "output.tar.gz")); os.IsNotExist(err) {
		t.Error("Archive file was not created")
	}
}

func TestEffectiveArchiveCompression(t *testing.T) {
	tests := []struct {
		format      string
		compression string
		want        string
	}{
		{format: "tar", want: "none"},
		{format: "squashfs", want: "gzip"},
		{format: "erofs", want: "none"},
		{format: "erofs", compression: "lz4hc", want: "lz4hc"},
	}
	for _, tt := range tests {
		t.Run(tt.format+"/"+tt.compression, func(t *testing.T) {
			if got := effectiveArchiveCompression(tt.format, tt.compression); got != tt.want {
				t.Errorf("effectiveArchiveCompression(%q, %q) = %q, want %q", tt.format, tt.compression, got, tt.want)
			}
		})
	}
}

func TestArchive_Apply_InvalidFormat(t *testing.T) {
	tmpDir := t.TempDir()

	a := &Archive{
		ID:     "test-archive",
		In:     "source",
		Out:    "output.zip",
		Format: "zip",
	}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error == nil {
		t.Error("Expected error for unsupported format")
	}
}
