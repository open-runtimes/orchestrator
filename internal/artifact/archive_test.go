package artifact

import (
	"archive/tar"
	"io"
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

// A post-job archive must record links as links. Opening one would follow an
// external runtime interpreter target in the sidecar filesystem, archive the
// target's bytes, and turn the restored virtualenv entry into the wrong file.
func TestArchive_Apply_PreservesSymlinkWithoutFollowing(t *testing.T) {
	tmpDir := t.TempDir()
	srcDir := filepath.Join(tmpDir, "source", "runtime-env", "bin")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkname := "/usr/local/bin/python3"
	if err := os.Symlink(linkname, filepath.Join(srcDir, "python3")); err != nil {
		t.Fatal(err)
	}

	a := &Archive{ID: "a", In: "source", Out: "output.tar", Format: "tar"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "success" {
		t.Fatalf("Apply() = %v, error = %v", result.Status, result.Error)
	}

	f, err := os.Open(filepath.Join(tmpDir, "output.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.ToSlash(header.Name) != "runtime-env/bin/python3" {
			continue
		}
		if header.Typeflag != tar.TypeSymlink || header.Linkname != linkname || header.Size != 0 {
			t.Fatalf("symlink header = type %d target %q size %d", header.Typeflag, header.Linkname, header.Size)
		}
		return
	}
	t.Fatal("python interpreter symlink missing from archive")
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

	if _, err := os.Stat(filepath.Join(tmpDir, "output.tar.gz")); os.IsNotExist(err) {
		t.Error("Archive file was not created")
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
