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
	"syscall"
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

// writeModeArchive builds a single-entry gzipped tar at tmpDir/test.tar.gz
// holding "package.json" with the given mode.
func writeModeArchive(t *testing.T, tmpDir string, mode int64) {
	t.Helper()

	file, err := os.Create(filepath.Join(tmpDir, "test.tar.gz"))
	if err != nil {
		t.Fatalf("Failed to create archive file: %v", err)
	}
	gzWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "package.json", Typeflag: tar.TypeReg, Mode: mode, Size: 2}); err != nil {
		t.Fatalf("Failed to write tar header: %v", err)
	}
	if _, err := tarWriter.Write([]byte("{}")); err != nil {
		t.Fatalf("Failed to write tar content: %v", err)
	}
	tarWriter.Close()
	gzWriter.Close()
	file.Close()
}

// TestUnarchive_Apply_NormalizesFileModes verifies extraction ignores the
// archive's own file modes. A source tarball built on the uploader's machine can
// carry an owner-only 0o600 entry, which the non-root sidecar could not read
// back when packing the build output — the build succeeded and then reported
// "permission denied" from the archive step.
//
// A restrictive umask is set throughout, since OpenFile's mode argument alone
// would be masked by it and these assertions would then pass or fail on the
// environment rather than on the extraction logic.
func TestUnarchive_Apply_NormalizesFileModes(t *testing.T) {
	defer syscall.Umask(syscall.Umask(0o077))

	for _, tc := range []struct {
		name string
		mode int64
		want fs.FileMode
	}{
		{"owner only", 0o600, 0o644},
		{"unreadable", 0, 0o644},
		{"executable", 0o700, 0o755},
		{"already permissive", 0o644, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			writeModeArchive(t, tmpDir, tc.mode)

			a := &Unarchive{ID: "test-unarchive-modes", In: "test.tar.gz", Out: "extracted"}
			if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
				t.Fatalf("Apply: %v", result.Error)
			}

			fi, err := os.Stat(filepath.Join(tmpDir, "extracted", "package.json"))
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Errorf("mode %#o extracted as %#o, want %#o", tc.mode, got, tc.want)
			}
		})
	}
}

// TestUnarchive_Apply_NormalizesExistingFileMode verifies a destination that
// already exists is re-moded too. OpenFile's mode argument applies only when it
// creates the file, so a truncating open over a path an earlier artifact (or a
// duplicate archive entry) already wrote would otherwise keep the stale mode and
// reintroduce the unreadable-source failure.
func TestUnarchive_Apply_NormalizesExistingFileMode(t *testing.T) {
	tmpDir := t.TempDir()
	writeModeArchive(t, tmpDir, 0o644)

	destDir := filepath.Join(tmpDir, "extracted")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	target := filepath.Join(destDir, "package.json")
	if err := os.WriteFile(target, []byte("stale"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := &Unarchive{ID: "test-unarchive-existing-mode", In: "test.tar.gz", Out: "extracted"}
	if result := a.Apply(t.Context(), tmpDir); result.Error != nil {
		t.Fatalf("Apply: %v", result.Error)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("pre-existing 0o600 destination left as %#o, want %#o", got, 0o644)
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

// TestUnarchive_Apply_UnreadableSource covers a source that opens but cannot be
// read. A directory is the portable way to produce that: os.Open succeeds and
// the read fails with EISDIR on both Linux and macOS. The read returns n == 0,
// so discarding its error would misreport a filesystem failure as an empty
// archive.
func TestUnarchive_Apply_UnreadableSource(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmpDir, "source.tar.gz"), 0o755); err != nil {
		t.Fatalf("Failed to create directory standing in for the archive: %v", err)
	}

	a := &Unarchive{ID: "test-unarchive-unreadable", In: "source.tar.gz", Out: "extracted"}

	result := a.Apply(t.Context(), tmpDir)
	if result.Error == nil {
		t.Fatal("expected error for unreadable source, got success")
	}
	for _, wrong := range []string{"is empty", "unrecognized archive format"} {
		if strings.Contains(result.Error.Error(), wrong) {
			t.Errorf("read failure misreported as %q: %v", wrong, result.Error)
		}
	}
	if !strings.Contains(result.Error.Error(), "failed to read archive header") {
		t.Errorf("expected the error to name the failed header read, got %v", result.Error)
	}
}

// createLinkArchive writes a tar.gz of regular files plus symlink/hard-link
// entries, mirroring how pnpm and npm ship a node_modules tree.
func createLinkArchive(t *testing.T, archiveIn string, files, symlinks, hardlinks map[string]string) {
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
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("header %s: %v", name, err)
		}
		if _, err := tarWriter.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for name, target := range symlinks {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name, Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: target,
		}); err != nil {
			t.Fatalf("symlink header %s: %v", name, err)
		}
	}
	for name, target := range hardlinks {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Typeflag: tar.TypeLink, Linkname: target,
		}); err != nil {
			t.Fatalf("hardlink header %s: %v", name, err)
		}
	}
}

// A pnpm node_modules is a tree of relative symlinks. Dropping them extracts a
// tree that looks complete and fails at runtime with a missing module, which is
// exactly how this surfaced in production.
func TestUnarchive_Apply_PreservesSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")

	createLinkArchive(t, archiveIn,
		map[string]string{
			"node_modules/.pnpm/react@19.2.6/node_modules/react/index.js": "module.exports = 'react';",
			"server/entry.mjs": "import 'react';",
		},
		map[string]string{
			"node_modules/react": ".pnpm/react@19.2.6/node_modules/react",
		},
		nil,
	)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "success" {
		t.Fatalf("Apply() = %v, error = %v", result.Status, result.Error)
	}

	link := filepath.Join(tmpDir, "out", "node_modules", "react")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("symlink not extracted: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected a symlink at %s, got mode %v", link, info.Mode())
	}
	// It must resolve to the real package, the way node's resolver walks it.
	if content, err := os.ReadFile(filepath.Join(link, "index.js")); err != nil {
		t.Fatalf("symlink does not resolve to the package: %v", err)
	} else if string(content) != "module.exports = 'react';" {
		t.Fatalf("resolved unexpected content: %q", content)
	}
}

func TestUnarchive_Apply_PreservesHardLinks(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")

	createLinkArchive(t, archiveIn,
		map[string]string{"pkg/real.js": "payload"},
		nil,
		map[string]string{"pkg/linked.js": "pkg/real.js"},
	)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "success" {
		t.Fatalf("Apply() = %v, error = %v", result.Status, result.Error)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "out", "pkg", "linked.js"))
	if err != nil {
		t.Fatalf("hard link not extracted: %v", err)
	}
	if string(content) != "payload" {
		t.Fatalf("hard link content = %q", content)
	}
}

// A link whose target escapes the destination must fail the artifact rather
// than plant a pointer to the host filesystem.
func TestUnarchive_Apply_RejectsEscapingLinks(t *testing.T) {
	for name, tc := range map[string]struct {
		symlinks  map[string]string
		hardlinks map[string]string
	}{
		"relative symlink": {symlinks: map[string]string{"evil": "../../../../etc/passwd"}},
		"absolute symlink": {symlinks: map[string]string{"evil": "/etc/passwd"}},
		"hard link":        {hardlinks: map[string]string{"evil": "../../../../etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			archiveIn := filepath.Join(tmpDir, "code.tar.gz")
			createLinkArchive(t, archiveIn, map[string]string{"keep.txt": "x"}, tc.symlinks, tc.hardlinks)

			a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
			result := a.Apply(t.Context(), tmpDir)
			if result.Status != "failed" {
				t.Fatalf("expected failure for an escaping link, got %v", result.Status)
			}
			if _, err := os.Lstat(filepath.Join(tmpDir, "out", "evil")); err == nil {
				t.Fatal("escaping link was created")
			}
		})
	}
}

// `a -> .` then `a/x -> ../escape`: the lexical parent (destDir/a) and the
// resolved one (destDir) disagree, so a purely lexical containment check lets
// the second link land outside the destination.
func TestUnarchive_Apply_RejectsChainedSymlinkEscape(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createLinkArchive(t, archiveIn,
		map[string]string{"keep.txt": "x"},
		// Order matters: the self-referential link is written first.
		map[string]string{"a": ".", "a/x": "../escape"},
		nil,
	)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "failed" {
		t.Fatalf("expected failure for a chained symlink escape, got %v", result.Status)
	}
	if _, err := os.Lstat(filepath.Join(tmpDir, "out", "x")); err == nil {
		t.Fatal("chained symlink escaped the destination")
	}
}

// An entry written beneath a symlinked directory would extract through the
// link, outside the destination.
func TestUnarchive_Apply_RejectsWritesThroughSymlinkedParent(t *testing.T) {
	tmpDir := t.TempDir()
	outside := filepath.Join(tmpDir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createLinkArchive(t, archiveIn,
		map[string]string{"a/planted.txt": "payload"},
		map[string]string{"a": "../outside"},
		nil,
	)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Status != "failed" {
		t.Fatalf("expected failure writing through a symlinked parent, got %v", result.Status)
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); err == nil {
		t.Fatal("file was planted outside the destination")
	}
}

// A rejected link must not have destroyed the entry it was replacing.
func TestUnarchive_Apply_RejectedLinkLeavesExistingFileIntact(t *testing.T) {
	tmpDir := t.TempDir()
	out := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(out, "keep.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createLinkArchive(t, archiveIn, nil, map[string]string{"keep.txt": "/etc/passwd"}, nil)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "failed" {
		t.Fatalf("expected failure for an absolute symlink, got %v", result.Status)
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("existing file was destroyed by a rejected entry: %v", err)
	}
	if string(content) != "original" {
		t.Fatalf("existing file = %q, want original", content)
	}
}

// A hard link names another entry of the same archive, so it has to follow the
// same strip/subdir rewrite as the file it points at.
func TestUnarchive_Apply_HardLinkHonoursStrip(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createLinkArchive(t, archiveIn,
		map[string]string{"wrapper/pkg/real.js": "payload"},
		nil,
		map[string]string{"wrapper/pkg/linked.js": "wrapper/pkg/real.js"},
	)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out", Strip: true}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "success" {
		t.Fatalf("Apply() = %v, error = %v", result.Status, result.Error)
	}
	content, err := os.ReadFile(filepath.Join(tmpDir, "out", "pkg", "linked.js"))
	if err != nil {
		t.Fatalf("hard link not extracted under strip: %v", err)
	}
	if string(content) != "payload" {
		t.Fatalf("hard link content = %q", content)
	}
}

// tarEntry is an archive entry with a defined position in the stream; several
// attacks depend on ordering, which a map cannot express.
type tarEntry struct {
	name     string
	typeflag byte
	body     string // contents for a regular file, link target otherwise
}

func createOrderedArchive(t *testing.T, archiveIn string, entries []tarEntry) {
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

	for _, e := range entries {
		header := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typeflag}
		switch e.typeflag {
		case tar.TypeReg:
			header.Size = int64(len(e.body))
		default:
			header.Linkname = e.body
			header.Mode = 0o777
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("header %s: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tarWriter.Write([]byte(e.body)); err != nil {
				t.Fatalf("write %s: %v", e.name, err)
			}
		}
	}
}

// `b -> .`, then `a -> b/../pwned`, then a regular file named `a`. Lexical
// cleaning cancels `b/..` and hides the escape, so the symlink is accepted and
// the file write follows it out of the destination. Order is the whole attack.
func TestUnarchive_Apply_RejectsSymlinkChainThroughParent(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createOrderedArchive(t, archiveIn, []tarEntry{
		{name: "b", typeflag: tar.TypeSymlink, body: "."},
		{name: "a", typeflag: tar.TypeSymlink, body: "b/../pwned"},
		{name: "a", typeflag: tar.TypeReg, body: "payload"},
	})

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "failed" {
		t.Fatalf("expected failure for a symlink chain, got %v", result.Status)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "pwned")); err == nil {
		t.Fatal("write escaped the destination")
	}
}

// A regular file must replace a symlink sitting at its name rather than being
// written through it.
func TestUnarchive_Apply_ReplacesSymlinkWithRegularFile(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createLinkArchive(t, archiveIn,
		map[string]string{"target.txt": "original", "link.txt": "replacement"},
		map[string]string{"link.txt": "target.txt"},
		nil,
	)

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "success" {
		t.Fatalf("Apply() = %v, error = %v", result.Status, result.Error)
	}
	// The symlink's target must be untouched by the later regular-file entry.
	content, err := os.ReadFile(filepath.Join(tmpDir, "out", "target.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original" {
		t.Fatalf("write followed the symlink: target.txt = %q", content)
	}
}

// A hard link whose source is missing must fail before replacing anything.
func TestUnarchive_Apply_MissingHardLinkSourceKeepsDestination(t *testing.T) {
	tmpDir := t.TempDir()
	out := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(out, "keep.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createLinkArchive(t, archiveIn, nil, nil, map[string]string{"keep.txt": "never/written.js"})

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "failed" {
		t.Fatalf("expected failure for a missing hard link source, got %v", result.Status)
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("destination destroyed by a failed hard link: %v", err)
	}
	if string(content) != "original" {
		t.Fatalf("keep.txt = %q, want original", content)
	}
}

// link(2) refuses a directory source, so the check has to happen before the
// destination is replaced.
func TestUnarchive_Apply_HardLinkToDirectoryKeepsDestination(t *testing.T) {
	tmpDir := t.TempDir()
	out := filepath.Join(tmpDir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(out, "keep.txt")
	if err := os.WriteFile(existing, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createOrderedArchive(t, archiveIn, []tarEntry{
		{name: "somedir/", typeflag: tar.TypeDir},
		{name: "keep.txt", typeflag: tar.TypeLink, body: "somedir"},
	})

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "failed" {
		t.Fatalf("expected failure for a directory hard-link source, got %v", result.Status)
	}
	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("destination destroyed by a failed hard link: %v", err)
	}
	if string(content) != "original" {
		t.Fatalf("keep.txt = %q, want original", content)
	}
}

// A hard link naming itself would have its source removed as the destination
// and then fail, destroying the file.
func TestUnarchive_Apply_SelfHardLinkKeepsSource(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createOrderedArchive(t, archiveIn, []tarEntry{
		{name: "keep.txt", typeflag: tar.TypeReg, body: "original"},
		{name: "keep.txt", typeflag: tar.TypeLink, body: "keep.txt"},
	})

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	if result := a.Apply(t.Context(), tmpDir); result.Status != "failed" {
		t.Fatalf("expected failure for a self-referential hard link, got %v", result.Status)
	}
	content, err := os.ReadFile(filepath.Join(tmpDir, "out", "keep.txt"))
	if err != nil {
		t.Fatalf("source destroyed by a self-referential hard link: %v", err)
	}
	if string(content) != "original" {
		t.Fatalf("keep.txt = %q, want original", content)
	}
}

// Per-entry validation is point-in-time, so a later entry can invalidate an
// earlier verdict: `x -> a/../escape` is contained while `a -> sub` names a
// real directory, and stops being contained once `sub` becomes a link to the
// root. Only the finished tree tells the truth.
func TestUnarchive_Apply_RejectsSymlinkMadeEscapingByLaterEntry(t *testing.T) {
	tmpDir := t.TempDir()
	archiveIn := filepath.Join(tmpDir, "code.tar.gz")
	createOrderedArchive(t, archiveIn, []tarEntry{
		{name: "sub/", typeflag: tar.TypeDir},
		{name: "a", typeflag: tar.TypeSymlink, body: "sub"},
		{name: "x", typeflag: tar.TypeSymlink, body: "a/../escape"},
		{name: "sub", typeflag: tar.TypeSymlink, body: "."},
	})

	a := &Unarchive{ID: "u", In: "code.tar.gz", Out: "out"}
	result := a.Apply(t.Context(), tmpDir)
	if result.Status != "failed" {
		t.Fatalf("expected failure once the tree leaves an escaping symlink, got %v", result.Status)
	}

	if result.Error == nil || !strings.Contains(result.Error.Error(), "escapes the destination after extraction") {
		t.Fatalf("expected the post-extraction check to be what rejected it, got %v", result.Error)
	}
}
