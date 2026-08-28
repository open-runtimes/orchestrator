package artifact

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/pierrec/lz4/v4"
)

// An extracted tree is materialized with our own permissions, never the
// archive's: the sidecar runs non-root, so a restrictive entry mode — 0o600 on
// a file the uploader kept private, 0o555 or 0o000 on a directory from a tar
// built on Windows — makes every later read fail with EACCES, and the archive
// step that packs the build output reports "permission denied" long after the
// build itself succeeded. Only the execute bit is carried over, since a source
// tree can legitimately ship executable scripts.
//
// Applied with an explicit Chmod rather than OpenFile's mode argument, which
// the umask masks and which a truncating open of an existing file — a duplicate
// archive entry, or a path an earlier artifact already wrote — ignores
// entirely, leaving the stale mode that this is meant to rule out.
const (
	extractDirMode  fs.FileMode = 0o755
	extractFileMode fs.FileMode = 0o644
)

// errUnsafeSymlink marks a symlink that cannot be materialized without
// pointing outside the extracted tree. Callers skip these entries for
// compatibility with archives produced before symlink extraction was added,
// while still surfacing every other extraction error.
var errUnsafeSymlink = errors.New("unsafe symlink target")

func extractMode(mode fs.FileMode) fs.FileMode {
	if mode.Perm()&0o111 != 0 {
		return extractFileMode | 0o111
	}
	return extractFileMode
}

// cleanSubdir normalizes a subdir filter to the slash-separated relative form
// archive entries use — "./astro/starter/" → "astro/starter" — so cosmetic
// prefixes don't miss every entry.
func cleanSubdir(s string) string {
	s = strings.Trim(path.Clean(s), "/")
	if s == "." {
		return ""
	}
	return s
}

// isGzip reports whether b starts with the gzip signature.
func isGzip(b []byte) bool {
	return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b
}

// isZstd reports whether b starts with the zstd signature.
func isZstd(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd
}

// isLz4 reports whether b starts with the lz4 frame signature.
func isLz4(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x04 && b[1] == 0x22 && b[2] == 0x4d && b[3] == 0x18
}

// isTar reports whether b holds the "ustar" signature at offset 257, which
// marks an uncompressed tar archive.
func isTar(b []byte) bool {
	return len(b) >= 262 && string(b[257:262]) == "ustar"
}

// Unarchive extracts a tar archive (plain, gzip, zstd, or lz4) or a squashfs or
// erofs image; the format is detected from the archive's magic bytes. (To mount
// a squashfs or erofs image read-only in place instead of materializing its
// files, use a "mount" artifact.)
type Unarchive struct {
	ID      string `json:"id"`
	In      string `json:"in"`               // Source archive path
	Out     string `json:"out"`              // Destination directory
	Subdir  string `json:"subdir,omitempty"` // Extract only this subdirectory
	Strip   bool   `json:"strip,omitempty"`  // Drop the archive's wrapper root directory
	Depends string `json:"depends,omitempty"`
}

func (a *Unarchive) ArtifactID() string   { return a.ID }
func (a *Unarchive) ArtifactType() string { return "unarchive" }
func (a *Unarchive) DependsOn() string    { return a.Depends }

// decompress wraps r for the named codec, returning the stream and a close
// function for whatever it allocated.
func decompress(r io.Reader, compression string) (io.Reader, func(), error) {
	switch compression {
	case "gzip":
		gzReader, err := pgzip.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		return gzReader, func() { gzReader.Close() }, nil
	case "zstd":
		zstdReader, err := zstd.NewReader(r)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create zstd reader: %w", err)
		}
		return zstdReader, zstdReader.Close, nil
	case "lz4":
		return lz4.NewReader(r), func() {}, nil
	default:
		return r, func() {}, nil
	}
}

// writeEntry materializes one archive entry. transform applies the archive's
// strip/subdir rewrite, which hard-link targets need as much as entry names do.
func writeEntry(header *tar.Header, tarReader io.Reader, targetPath, destDir string, transform func(string) (string, bool)) error {
	switch header.Typeflag {
	case tar.TypeDir:
		return mkdirAllInRoot(destDir, targetPath)

	case tar.TypeReg:
		return writeRegularFile(header, tarReader, targetPath, destDir)

	case tar.TypeSymlink, tar.TypeLink:
		// pnpm builds node_modules almost entirely from symlinks, and npm links
		// every .bin entry, so dropping these silently ships a tree that looks
		// complete and fails at runtime with a missing module.
		linkSource := ""
		if header.Typeflag == tar.TypeLink {
			source, ok := transform(filepath.Clean(strings.TrimPrefix(header.Linkname, "./")))
			if !ok {
				return fmt.Errorf("hard link target excluded by strip/subdir: %s -> %s", header.Name, header.Linkname)
			}
			linkSource = filepath.Join(destDir, source)
		}
		err := extractLink(header, targetPath, destDir, linkSource)
		if header.Typeflag == tar.TypeSymlink && errors.Is(err, errUnsafeSymlink) {
			slog.Warn("Skipping symlink outside archive destination",
				"name", header.Name,
				"target", header.Linkname,
				"error", err,
			)
			return nil
		}
		return err

	default:
		slog.Debug("Skipping archive entry", "name", header.Name, "type", header.Typeflag)
		return nil
	}
}

// writeRegularFile materializes one file entry atomically: the bytes go to a
// temporary file alongside the target and are renamed over it only once they
// are all there.
//
// Atomic because the workspace is shared and outlives a failed artifact — a
// truncating write that dies mid-copy leaves a half file where consumers
// expect whole ones. rename(2) also replaces a symlink sitting at the name
// rather than following it, which is what stops an earlier entry redirecting
// this write out of the destination.
func writeRegularFile(header *tar.Header, tarReader io.Reader, targetPath, destDir string) error {
	if err := mkdirAllInRoot(destDir, filepath.Dir(targetPath)); err != nil {
		return err
	}

	mode := extractMode(os.FileMode(header.Mode))
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".artifact-*")
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	tmpName := tmp.Name()
	// Cleans up on every failure path; after a successful rename the name is
	// gone and both calls are harmless no-ops.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("failed to set file mode: %w", err)
	}
	if _, err := io.Copy(tmp, tarReader); err != nil {
		return fmt.Errorf("failed to extract file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to extract file: %w", err)
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	return nil
}

// mkdirAllInRoot creates dir and every missing parent between it and root,
// refusing to traverse a symlink on the way down. Without that refusal an
// archive could ship `a -> /elsewhere` followed by entries under `a/`, and the
// extraction would write through the link — outside the workspace — even though
// every path involved looks contained when compared lexically.
func mkdirAllInRoot(root, dir string) error {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive entry escapes the destination: %s", dir)
	}
	if err := os.MkdirAll(filepath.Clean(root), extractDirMode); err != nil {
		return fmt.Errorf("failed to create destination: %w", err)
	}
	if rel == "." {
		return nil
	}

	current := filepath.Clean(root)
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case os.IsNotExist(err):
			if err := os.Mkdir(current, extractDirMode); err != nil && !os.IsExist(err) {
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case err != nil:
			return fmt.Errorf("failed to inspect path: %w", err)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("archive path traverses a symlink: %s", current)
		case !info.IsDir():
			return fmt.Errorf("archive path traverses a file: %s", current)
		}
	}
	return nil
}

// extractLink recreates a symlink or hard link from the archive.
//
// Every target is validated BEFORE anything existing is replaced, so a rejected
// entry cannot leave the tree missing the file it was going to overwrite. The
// link is written with the archive's own linkname, so an absolute target is
// marked unsafe rather than rewritten: "containing" it by validating a
// rewritten path would still plant a pointer outside the workspace. The caller
// skips unsafe symlinks for compatibility with legacy archives. Parents are
// created by mkdirAllInRoot, which refuses to traverse a symlink — that is what
// stops a chain like `a -> .` followed by `a/x -> ../escape`, where the lexical
// parent and the resolved one disagree.
func extractLink(header *tar.Header, targetPath, destDir, hardLinkSource string) error {
	// Resolved, because the walk below resolves too and the two have to be
	// comparable — on macOS a temp dir under /var really lives in /private/var.
	root := filepath.Clean(destDir)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	if header.Typeflag == tar.TypeLink {
		if err := validateHardLinkSource(header, targetPath, destDir, hardLinkSource, root); err != nil {
			return err
		}
	} else if err := validateSymlinkTarget(header, targetPath, destDir, root); err != nil {
		return err
	}

	if err := mkdirAllInRoot(destDir, filepath.Dir(targetPath)); err != nil {
		return err
	}
	// Only now that the entry is known good: a re-extraction over an existing
	// tree must not fail on a link that is already there, and the archive is the
	// source of truth for what it points at.
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to replace existing entry: %w", err)
	}

	if header.Typeflag == tar.TypeLink {
		if err := os.Link(hardLinkSource, targetPath); err != nil {
			return fmt.Errorf("failed to create hard link: %w", err)
		}
		return nil
	}
	if err := os.Symlink(header.Linkname, targetPath); err != nil {
		return fmt.Errorf("failed to create symlink: %w", err)
	}
	return nil
}

// validateHardLinkSource checks a hard link before anything is replaced: the
// source has to be contained, present, and linkable. A source that comes later
// in the stream (or never), or that names a directory link(2) will refuse,
// would otherwise delete the destination and only then fail.
func validateHardLinkSource(header *tar.Header, targetPath, destDir, hardLinkSource, root string) error {
	if !underRoot(resolveWalking(destDir, relativeTo(destDir, hardLinkSource)), root) {
		return fmt.Errorf("invalid hard link target in archive: %s -> %s", header.Name, header.Linkname)
	}
	// A self-referential link would have its source removed as the destination
	// and then fail, taking the file with it.
	if resolveIfPossible(hardLinkSource) == resolveIfPossible(targetPath) {
		return fmt.Errorf("hard link points at itself: %s -> %s", header.Name, header.Linkname)
	}
	info, err := os.Lstat(hardLinkSource)
	if err != nil {
		return fmt.Errorf("hard link source not found in archive: %s -> %s", header.Name, header.Linkname)
	}
	if info.IsDir() {
		return fmt.Errorf("hard link source is a directory: %s -> %s", header.Name, header.Linkname)
	}
	// link(2) does not follow symlinks: it would duplicate the link inode, not
	// the file. That is how an alias of the destination — or a dangling one,
	// which cannot be compared against it at all — turns an extraction into a
	// self-referential tree. No build tool emits this, so refuse it.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("hard link source is a symlink: %s -> %s", header.Name, header.Linkname)
	}
	return nil
}

// resolveIfPossible resolves path through symlinks when it exists, and cleans
// it otherwise, so two names for one file compare equal.
func resolveIfPossible(target string) string {
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		return resolved
	}
	return filepath.Clean(target)
}

// validateSymlinkTarget marks a target that leaves the destination as unsafe,
// except for the narrowly recognized Python runtime interpreter chain. An
// absolute target is not rewritten: the link is written with the archive's own
// linkname, so validating a rewritten path would still plant a pointer outside
// the workspace.
func validateSymlinkTarget(header *tar.Header, targetPath, destDir, root string) error {
	if isRuntimePythonSymlink(targetPath, destDir, header.Linkname) {
		return nil
	}
	if filepath.IsAbs(header.Linkname) {
		return fmt.Errorf("%w: absolute symlink target in archive: %s -> %s", errUnsafeSymlink, header.Name, header.Linkname)
	}
	if !underRoot(resolveWalking(filepath.Dir(targetPath), header.Linkname), root) {
		return fmt.Errorf("%w: invalid symlink target in archive: %s -> %s", errUnsafeSymlink, header.Name, header.Linkname)
	}
	return nil
}

// isRuntimePythonSymlink recognizes the one external-link shape required by
// Python build artifacts. python -m venv writes runtime-env/bin/python3 as an
// absolute link to the interpreter used for the build (the Open Runtimes
// images install it under /usr/local/bin), then makes python and the versioned
// alias point at that link. Dropping the absolute link leaves both aliases
// dangling and the runtime crashes before it can start.
//
// This is deliberately narrower than allowing arbitrary absolute symlinks:
// only Python interpreter names at the canonical runtime-env/bin location may
// point at an interpreter in a system bin directory shared by the matching
// build and runtime images. mkdirAllInRoot still refuses to traverse the link
// while extracting later entries, so accepting this leaf cannot redirect an
// archive write outside the workspace.
func isRuntimePythonSymlink(linkPath, root, linkname string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(linkPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != "runtime-env" || parts[1] != "bin" || !isPythonInterpreter(parts[2]) {
		return false
	}

	if !filepath.IsAbs(linkname) {
		return filepath.Base(linkname) == linkname && isPythonInterpreter(linkname)
	}
	if filepath.Clean(linkname) != linkname {
		return false
	}
	targetDir := filepath.ToSlash(filepath.Dir(linkname))
	if targetDir != "/usr/local/bin" && targetDir != "/usr/bin" {
		return false
	}
	return isPythonInterpreter(filepath.Base(linkname))
}

func isPythonInterpreter(name string) bool {
	if name == "python" {
		return true
	}
	if !strings.HasPrefix(name, "python") {
		return false
	}
	suffix := strings.TrimPrefix(name, "python")
	if suffix == "" {
		return true
	}
	hasDigit := false
	for _, c := range suffix {
		if c >= '0' && c <= '9' {
			hasDigit = true
			continue
		}
		if c != '.' {
			return false
		}
	}
	return hasDigit
}

// resolveWalking follows name from base one component at a time, resolving any
// symlink it lands on before taking the next step.
//
// filepath.Join cannot be used for this: it cleans lexically, so `b/../x`
// collapses to `x` before anything notices that `b` is a symlink pointing
// somewhere else — which is precisely how a chain of links escapes a
// lexical containment check. Walking makes the escape visible.
func resolveWalking(base, name string) string {
	current := base
	if resolved, err := filepath.EvalSymlinks(current); err == nil {
		current = resolved
	}

	for part := range strings.SplitSeq(filepath.ToSlash(name), "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
		default:
			current = filepath.Join(current, part)
		}
		// Only an existing component can be resolved; the last one usually is
		// not there yet, and an unresolvable component is judged as written.
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			current = resolved
		}
	}
	return current
}

// relativeTo expresses target relative to base, for feeding back into a walk.
func relativeTo(base, target string) string {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return target
	}
	return rel
}

// underRoot reports whether candidate is root itself or sits beneath it.
func underRoot(candidate, root string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// Apply extracts the archive, detecting squashfs / plain tar / gzip / zstd / lz4 from
// its magic bytes. If Strip is set, the first path component of every entry
// is dropped — git-forge archives (GitHub's "{repo}-{ref}/", Gitea's
// "{repo}/") wrap the tree in a single root directory whose name the caller
// can't always predict. If Subdir is specified, only files under that
// subdirectory are extracted, with the subdir prefix stripped; with Strip
// the subdir is resolved against the unwrapped tree, without it (legacy) a
// tar's detected root folder is implicitly prepended to the subdir.
func (a *Unarchive) Apply(ctx context.Context, basePath string) *Result {
	srcPath := filepath.Join(basePath, a.In)
	destDir := filepath.Join(basePath, a.Out)

	// Read enough to cover every magic we sniff. squashfs "hsqs" sits at offset
	// 0 and tar "ustar" at 257, but erofs's magic is at offset 1024 — so read
	// past it (1028 bytes) rather than the 512 the tar/squashfs checks needed.
	// A source that is missing or empty has no magic to sniff, so it would
	// otherwise fall through to "unrecognized archive format" and mask the real
	// failure (typically the download artifact that was meant to produce it).
	header := make([]byte, 1028)
	f, err := os.Open(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to open archive: %w", err)}
	}
	n, err := io.ReadFull(f, header)
	f.Close()

	// A short read is expected — a small archive is legitimately shorter than
	// the header we sniff — but any other read error is a filesystem failure
	// that must not be reported as an empty or unrecognized archive.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to read archive header: %w", err)}
	}
	header = header[:n]

	if n == 0 {
		return &Result{Status: "failed", Error: fmt.Errorf("archive %s is empty", a.In)}
	}

	format, compression := Classify(header)

	switch format {
	case "squashfs":
		if err := extractSquashfs(srcPath, destDir, a.Subdir, a.Strip); err != nil {
			return &Result{Status: "failed", Error: err}
		}
		slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "strip", a.Strip, "format", format, "compression", compression)
		return &Result{Status: "success", Format: format, Compression: compression}
	case "erofs":
		if err := extractErofs(srcPath, destDir, a.Subdir, a.Strip); err != nil {
			return &Result{Status: "failed", Error: err}
		}
		slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "strip", a.Strip, "format", format)
		return &Result{Status: "success", Format: format}
	case "tar":
		return a.extractTar(srcPath, destDir, compression)
	default:
		return &Result{Status: "failed", Error: fmt.Errorf("unrecognized archive format for %s", a.In)}
	}
}

// extractTar extracts a tar archive at srcPath into destDir, decompressing the
// stream first with the named codec ("gzip", "zstd", "lz4", or "none" for plain
// tar). The codec is echoed on the Result so callers learn what it actually was.
func (a *Unarchive) extractTar(srcPath, destDir, compression string) (result *Result) {
	// Deferred so it runs on every exit, not just the successful one: an entry
	// that fails partway through may follow one that already planted an
	// escaping link, and returning early would leave it on a workspace that
	// outlives this artifact. The sweep removes what it finds; it only becomes
	// the reported error when there is not already one.
	defer func() {
		if _, statErr := os.Stat(destDir); statErr != nil {
			return
		}
		sweepErr := assertNoEscapingSymlinks(destDir)
		if sweepErr == nil {
			return
		}
		// Joined rather than dropped when extraction already failed: the sweep
		// reports what it could not remove, and a workspace still holding a
		// pointer out of itself must not be masked by whatever failed first.
		if result != nil && result.Error != nil {
			result = &Result{Status: "failed", Error: errors.Join(result.Error, sweepErr)}
			return
		}
		result = &Result{Status: "failed", Error: sweepErr}
	}()

	file, err := os.Open(srcPath)
	if err != nil {
		return &Result{Status: "failed", Error: fmt.Errorf("failed to open archive: %w", err)}
	}
	defer file.Close()

	src, closeStream, err := decompress(file, compression)
	if err != nil {
		return &Result{Status: "failed", Error: err}
	}
	defer closeStream()

	tarReader := tar.NewReader(src)

	subdir := cleanSubdir(a.Subdir)
	var archiveRoot string

	extracted := 0
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Result{Status: "failed", Error: fmt.Errorf("failed to read tar header: %w", err)}
		}

		// GitHub git-archive tarballs open with a pax global header (typeflag 'g')
		// carrying the commit SHA. Go's tar reader surfaces it as a real entry —
		// unlike Python, which consumes it transparently — and its synthetic
		// "pax_global_header" name would otherwise be mistaken for the archive
		// root, so a subdir match against "pax_global_header/<subdir>" skips every
		// real file. Skip global/extended headers; Go already applies their
		// metadata to the following entry.
		if header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeXHeader {
			continue
		}

		cleanName := filepath.Clean(header.Name)
		if strings.HasPrefix(cleanName, "..") {
			return &Result{Status: "failed", Error: fmt.Errorf("invalid path in archive: %s", header.Name)}
		}

		// Applied to entry names and to hard-link targets alike: a link names
		// another entry of the same archive, so it has to survive the same
		// strip/subdir rewrite or it points at a path that was never written.
		transform := func(name string) (string, bool) {
			out := name
			if a.Strip {
				parts := strings.SplitN(name, "/", 2)
				if len(parts) < 2 {
					return "", false // the wrapper root directory entry itself
				}
				out = parts[1]
			}

			if subdir != "" {
				prefix := subdir
				if !a.Strip {
					// Legacy: resolve subdir under the tar's detected root folder.
					if archiveRoot == "" {
						archiveRoot = strings.SplitN(name, "/", 2)[0]
					}
					prefix = archiveRoot + "/" + subdir
				}

				if !strings.HasPrefix(out, prefix+"/") && out != prefix {
					return "", false
				}

				out = strings.TrimPrefix(out, prefix)
				out = strings.TrimPrefix(out, "/")
				if out == "" {
					return "", false
				}
			}
			return out, true
		}

		extractPath, ok := transform(cleanName)
		if !ok {
			continue
		}

		targetPath := filepath.Join(destDir, extractPath)
		extracted++

		if err := writeEntry(header, tarReader, targetPath, destDir, transform); err != nil {
			return &Result{Status: "failed", Error: err}
		}
	}

	// strip on a flat archive (or a subdir that matches nothing) would
	// otherwise succeed with an empty destination — a hard-to-trace "no
	// source code" failure at whatever consumes the output. Fail here, where
	// the cause is still visible.
	if extracted == 0 && (a.Strip || subdir != "") {
		return &Result{Status: "failed", Error: fmt.Errorf("no entries extracted from %s (strip=%t, subdir=%q): archive layout does not match", a.In, a.Strip, subdir)}
	}

	slog.Debug("Extracted archive", "src", srcPath, "dest", destDir, "subdir", a.Subdir, "strip", a.Strip, "compression", compression)
	return &Result{Status: "success", Format: "tar", Compression: compression}
}

// assertNoEscapingSymlinks walks the extracted tree and removes any symlink
// resolving outside destDir except the recognized Python runtime interpreter
// chain. It fails only when the tree cannot be completely inspected or an
// unsafe link cannot be removed. WalkDir does not follow symlinks, so the walk
// itself cannot be led astray.
func assertNoEscapingSymlinks(destDir string) error {
	root := filepath.Clean(destDir)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	var escaped, problems []string
	// Nothing here aborts the walk. A sweep that stops at the first unreadable
	// entry would leave every escaping link after it in place — the opposite of
	// the point — so failures are collected and the walk always completes.
	_ = filepath.WalkDir(destDir, func(entry string, d fs.DirEntry, walkErr error) error {
		removed, problem := pruneIfEscaping(entry, d, walkErr, root, destDir)
		if removed != "" {
			escaped = append(escaped, removed)
		}
		if problem != "" {
			problems = append(problems, problem)
		}
		return nil
	})

	switch {
	case len(problems) > 0 && len(escaped) > 0:
		return fmt.Errorf("symlinks escape the destination after extraction (removed: %s; unresolved: %s)", strings.Join(escaped, ", "), strings.Join(problems, "; "))
	case len(problems) > 0:
		return fmt.Errorf("could not verify the extracted tree for escaping symlinks: %s", strings.Join(problems, "; "))
	case len(escaped) > 0:
		slog.Warn("Removed symlinks outside archive destination",
			"links", strings.Join(escaped, ", "),
		)
	}
	return nil
}

// pruneIfEscaping removes entry when it is a symlink resolving outside root,
// reporting what it removed or what stopped it. It never fails the walk: the
// caller needs every entry inspected, not an early exit.
func pruneIfEscaping(entry string, d fs.DirEntry, walkErr error, root, destDir string) (removed, problem string) {
	if walkErr != nil {
		return "", entry + ": " + walkErr.Error()
	}
	if d.Type()&fs.ModeSymlink == 0 {
		return "", ""
	}
	linkname, err := os.Readlink(entry)
	if err != nil {
		return "", entry + ": " + err.Error()
	}
	if isRuntimePythonSymlink(entry, destDir, linkname) {
		return "", ""
	}
	if underRoot(resolveWalking(filepath.Dir(entry), linkname), root) {
		return "", ""
	}
	// Removed, not just reported: the workspace is shared and outlives this
	// artifact, so even a compatible extraction must not leave a usable pointer
	// out of it behind. Removing a symlink unlinks the link, never its target.
	if err := os.Remove(entry); err != nil {
		return "", "failed to remove escaping symlink " + entry + " -> " + linkname + ": " + err.Error()
	}
	return entry + " -> " + linkname, ""
}

// extractFS materializes the contents of an image filesystem (squashfs or
// erofs, both exposed as an fs.FS) into destDir. Strip drops each entry's first
// path component; Subdir extracts only entries under that prefix, with the
// prefix removed. Shared by extractSquashfs and extractErofs.
func extractFS(fsys fs.FS, destDir, subdir string, strip bool) error {
	subdir = cleanSubdir(subdir)

	extracted := 0
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}

		rel := p
		if strip {
			parts := strings.SplitN(p, "/", 2)
			if len(parts) < 2 {
				return nil // the wrapper root directory entry itself
			}
			rel = parts[1]
		}
		if subdir != "" {
			if rel != subdir && !strings.HasPrefix(rel, subdir+"/") {
				return nil
			}
			rel = strings.TrimPrefix(strings.TrimPrefix(rel, subdir), "/")
			if rel == "" {
				return nil
			}
		}

		// Materialize only directories and regular files, matching extractTar.
		// Symlinks, FIFOs, devices, and sockets in an image are skipped rather
		// than mis-written as regular files (or failing the io.Copy below).
		if !d.IsDir() && !d.Type().IsRegular() {
			slog.Debug("Skipping non-regular archive entry", "name", p, "type", d.Type())
			return nil
		}

		target := filepath.Join(destDir, rel)
		extracted++
		if d.IsDir() {
			return os.MkdirAll(target, extractDirMode)
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), extractDirMode); err != nil {
			return fmt.Errorf("failed to create parent directory: %w", err)
		}

		entry, err := fsys.Open(p)
		if err != nil {
			return fmt.Errorf("failed to open entry: %w", err)
		}
		defer entry.Close()

		mode := extractMode(fi.Mode())
		outFile, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		if err := outFile.Chmod(mode); err != nil {
			outFile.Close()
			return fmt.Errorf("failed to set file mode: %w", err)
		}
		if _, err := io.Copy(outFile, entry); err != nil {
			outFile.Close()
			return fmt.Errorf("failed to extract file: %w", err)
		}
		return outFile.Close()
	})
	if err != nil {
		return err
	}

	// See extractTar: an empty result from strip/subdir filtering is a
	// misconfiguration, not a success.
	if extracted == 0 && (strip || subdir != "") {
		return fmt.Errorf("no entries extracted (strip=%t, subdir=%q): archive layout does not match", strip, subdir)
	}
	return nil
}
