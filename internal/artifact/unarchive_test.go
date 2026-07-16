package artifact

import (
	"archive/tar"
	"compress/gzip"
	"io"
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
	tmpDir := t.TempDir()

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

	result := a.Apply(t.Context(), tmpDir)
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
	tmpDir := t.TempDir()

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

	result := a.Apply(t.Context(), tmpDir)
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

// A cosmetic "./" prefix (or trailing slash) on subdir must not miss every
// archive entry — the filter is normalized to the entries' relative form.
func TestUnarchive_Apply_Subdir_DotSlash(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"repo-main/astro/starter/index.astro": "astro code",
		"repo-main/other/main.py":             "python code",
	})

	a := &Unarchive{
		ID:     "test-unarchive-dotslash",
		In:     "test.tar.gz",
		Out:    "extracted",
		Subdir: "./astro/starter/",
	}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "index.astro"))
	if err != nil {
		t.Fatalf("Failed to read index.astro: %v", err)
	}
	if string(content) != "astro code" {
		t.Errorf("Expected 'astro code', got %q", string(content))
	}
}

// TestUnarchive_Apply_Subdir_PaxGlobalHeader reproduces a GitHub git-archive
// tarball: a leading pax global header (typeflag 'g', carrying the commit SHA)
// followed by a rooted tree. The global header must not be mistaken for the
// archive root, or the subdir match skips every real file and nothing extracts.
func TestUnarchive_Apply_Subdir_PaxGlobalHeader(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	file, err := os.Create(archiveIn)
	if err != nil {
		t.Fatalf("Failed to create archive file: %v", err)
	}
	gzWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzWriter)

	if err := tarWriter.WriteHeader(&tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": "0123456789abcdef0123456789abcdef01234567"},
	}); err != nil {
		t.Fatalf("Failed to write global header: %v", err)
	}
	for name, content := range map[string]string{
		"templates-main/node/starter/index.js": "node code",
		"templates-main/python/main.py":        "python code",
	} {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("Failed to write tar header: %v", err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("Failed to write tar content: %v", err)
		}
	}
	for _, c := range []io.Closer{tarWriter, gzWriter, file} {
		if err := c.Close(); err != nil {
			t.Fatalf("Failed to close writer: %v", err)
		}
	}

	a := &Unarchive{ID: "ua", In: "test.tar.gz", Out: "extracted", Subdir: "node/starter"}
	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
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

// TestUnarchive_Apply_Strip unwraps a git-forge archive whose tree sits
// inside a single root directory (Gitea's "{repo}/", GitHub's "{repo}-{ref}/")
// without the caller having to know that directory's name.
func TestUnarchive_Apply_Strip(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"repo/README.md":        "readme",
		"repo/src/index.js":     "node code",
		"repo/src/package.json": "{}",
	})

	a := &Unarchive{
		ID:    "test-unarchive-strip",
		In:    "test.tar.gz",
		Out:   "extracted",
		Strip: true,
	}

	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	extractedDir := filepath.Join(tmpDir, "extracted")
	if got, err := os.ReadFile(filepath.Join(extractedDir, "README.md")); err != nil || string(got) != "readme" {
		t.Fatalf("README.md = %q, err = %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(extractedDir, "src", "index.js")); err != nil || string(got) != "node code" {
		t.Fatalf("src/index.js = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(extractedDir, "repo")); !os.IsNotExist(err) {
		t.Error("wrapper root directory should not exist in extracted directory")
	}
}

// TestUnarchive_Apply_Strip_Subdir resolves subdir against the unwrapped
// tree, unlike the legacy behavior where a detected root is prepended.
func TestUnarchive_Apply_Strip_Subdir(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"repo/README.md":                "readme",
		"repo/functions/node/index.js":  "node code",
		"repo/functions/python/main.py": "python code",
	})

	a := &Unarchive{
		ID:     "test-unarchive-strip-subdir",
		In:     "test.tar.gz",
		Out:    "extracted",
		Subdir: "functions/node",
		Strip:  true,
	}

	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
		t.Fatalf("Apply() error = %v", result.Error)
	}

	extractedDir := filepath.Join(tmpDir, "extracted")
	if got, err := os.ReadFile(filepath.Join(extractedDir, "index.js")); err != nil || string(got) != "node code" {
		t.Fatalf("index.js = %q, err = %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(extractedDir, "main.py")); !os.IsNotExist(err) {
		t.Error("main.py should not exist in extracted directory")
	}
}

// TestUnarchive_Apply_Strip_FlatArchive fails loudly when strip is used on an
// archive with no wrapper directory: every entry sits at the root, so stripping
// the first path component would silently extract nothing.
func TestUnarchive_Apply_Strip_FlatArchive(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"file.txt": "content",
	})

	a := &Unarchive{
		ID:    "test-unarchive-strip-flat",
		In:    "test.tar.gz",
		Out:   "extracted",
		Strip: true,
	}

	if result := a.Apply(t.Context(), tmpDir); result.Error == nil {
		t.Fatal("expected error for strip on a flat archive, got success")
	}
}

// TestUnarchive_Apply_Subdir_NoMatch fails loudly when subdir matches nothing,
// instead of succeeding with an empty destination.
func TestUnarchive_Apply_Subdir_NoMatch(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "test.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"repo-main/README.md": "readme",
	})

	a := &Unarchive{
		ID:     "test-unarchive-subdir-nomatch",
		In:     "test.tar.gz",
		Out:    "extracted",
		Subdir: "does/not/exist",
	}

	if result := a.Apply(t.Context(), tmpDir); result.Error == nil {
		t.Fatal("expected error for unmatched subdir, got success")
	}
}

func TestUnarchive_Apply_InTraversal(t *testing.T) {
	tmpDir := t.TempDir()

	archiveIn := filepath.Join(tmpDir, "malicious.tar.gz")
	createTestArchive(t, archiveIn, map[string]string{
		"../../../etc/passwd": "malicious content",
	})

	a := &Unarchive{
		ID:  "test-unarchive",
		In:  "malicious.tar.gz",
		Out: "extracted",
	}

	result := a.Apply(t.Context(), tmpDir)
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
