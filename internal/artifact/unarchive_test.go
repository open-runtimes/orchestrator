package artifact

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// TestExtractFS_SkipsNonRegular verifies the shared image extractor writes only
// directories and regular files, skipping symlinks and other special entries
// (symmetric with extractTar) rather than materializing them as regular files.
func TestExtractFS_SkipsNonRegular(t *testing.T) {
	src := fstest.MapFS{
		"dir/file.txt": {Data: []byte("real"), Mode: 0o644},
		"link":         {Mode: fs.ModeSymlink, Data: []byte("dir/file.txt")},
		"pipe":         {Mode: fs.ModeNamedPipe},
	}
	dest := t.TempDir()
	if err := extractFS(src, dest, "", false); err != nil {
		t.Fatalf("extractFS() error = %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dest, "dir", "file.txt")); err != nil || string(got) != "real" {
		t.Fatalf("dir/file.txt = %q, err = %v", got, err)
	}
	for _, skipped := range []string{"link", "pipe"} {
		if _, err := os.Lstat(filepath.Join(dest, skipped)); !os.IsNotExist(err) {
			t.Errorf("%s should have been skipped, got err = %v", skipped, err)
		}
	}
}

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

// TestUnarchive_Apply_UnwritableDir covers archives whose directory entries are
// not owner-writable (0o555, or 0o000 from Windows tooling): extraction of
// anything nested inside them must still succeed, because the sidecar runs as a
// non-root user that cannot write into such a directory.
func TestUnarchive_Apply_UnwritableDir(t *testing.T) {
	for _, dirMode := range []int64{0o555, 0o500, 0} {
		tmpDir := t.TempDir()
		archiveIn := filepath.Join(tmpDir, "test.tar.gz")

		file, err := os.Create(archiveIn)
		if err != nil {
			t.Fatalf("Failed to create archive file: %v", err)
		}
		gzWriter := gzip.NewWriter(file)
		tarWriter := tar.NewWriter(gzWriter)
		for _, h := range []*tar.Header{
			{Name: "src", Typeflag: tar.TypeDir, Mode: dirMode},
			{Name: "src/admin-frontend", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "src/admin-frontend/main.ts", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4},
		} {
			if err := tarWriter.WriteHeader(h); err != nil {
				t.Fatalf("Failed to write tar header: %v", err)
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := tarWriter.Write([]byte("code")); err != nil {
					t.Fatalf("Failed to write tar content: %v", err)
				}
			}
		}
		tarWriter.Close()
		gzWriter.Close()
		file.Close()

		a := &Unarchive{ID: "test-unarchive-unwritable", In: "test.tar.gz", Out: "extracted"}
		if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
			t.Fatalf("dir mode %#o: %v", dirMode, result.Error)
		}

		got, err := os.ReadFile(filepath.Join(tmpDir, "extracted", "src", "admin-frontend", "main.ts"))
		if err != nil {
			t.Fatalf("dir mode %#o: reading extracted file: %v", dirMode, err)
		}
		if string(got) != "code" {
			t.Errorf("dir mode %#o: got %q, want %q", dirMode, got, "code")
		}
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

// TestUnarchive_Apply_MissingSource covers the case where the archive was never
// produced, typically because the download artifact meant to fetch it failed.
// The error must name the missing file rather than blaming its format, which is
// what a silently-ignored os.Open would report.
func TestUnarchive_Apply_MissingSource(t *testing.T) {
	a := &Unarchive{ID: "test-unarchive-missing", In: "source.tar.gz", Out: "extracted"}

	result := a.Apply(t.Context(), t.TempDir())
	if result.Error == nil {
		t.Fatal("expected error for missing source, got success")
	}
	if !os.IsNotExist(errors.Unwrap(result.Error)) {
		t.Errorf("expected a not-exist error, got %v", result.Error)
	}
	if strings.Contains(result.Error.Error(), "unrecognized archive format") {
		t.Errorf("missing source misreported as a format problem: %v", result.Error)
	}
}

// TestUnarchive_Apply_EmptySource covers a zero-byte archive, which has no magic
// bytes to sniff and would otherwise be reported as an unrecognized format.
func TestUnarchive_Apply_EmptySource(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "source.tar.gz"), nil, 0o644); err != nil {
		t.Fatalf("Failed to write empty archive: %v", err)
	}

	a := &Unarchive{ID: "test-unarchive-empty", In: "source.tar.gz", Out: "extracted"}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error == nil {
		t.Fatal("expected error for empty source, got success")
	}
	if !strings.Contains(result.Error.Error(), "empty") {
		t.Errorf("expected the error to call the archive empty, got %v", result.Error)
	}
}
