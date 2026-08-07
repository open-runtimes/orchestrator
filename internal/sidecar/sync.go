package sidecar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"orchestrator/internal/artifact"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A synced mount keeps its workspace across sandboxes without the orchestrator
// owning any storage. The image is the base; the overlay's upper layer is
// everything the workload changed, and that delta is what travels:
//
//	establish  restore the delta, then stack the overlay over the image
//	running    push the delta every syncIntervalSeconds
//	teardown   push it once more, after the drain, before unmounting
//
// Continuous sync is what makes the last push an optimisation rather than a
// promise: missing it costs the interval, not the session. Nothing here is
// atomic — the workload keeps writing while the delta is archived — so an
// intermediate push is a crash-consistent restore point. The final one runs
// when nothing is serving, so it is clean.

// syncState tracks the loops started for a claim's synced mounts, and what each
// one last pushed.
type syncState struct {
	mu      sync.Mutex
	stop    chan struct{} // closed by StopSync; ends every loop
	done    sync.WaitGroup
	mounts  []*artifact.Mount
	pushed  map[string]deltaPrint // mount id → what its last successful push carried
	stopped bool
}

// deltaPrint is a cheap summary of the upper layer — enough to tell whether
// anything changed since the last push without reading a byte of content, so a
// short interval costs a directory walk rather than an archive and an upload.
//
// Nanosecond mtimes make this reliable in practice: an in-place edit that
// preserves the file's size AND lands in the same nanosecond as the last push
// would be missed, and the next change to anything catches it.
type deltaPrint struct {
	files  int
	bytes  int64
	newest int64 // newest mtime, UnixNano
}

// printDelta summarises a directory tree. A tree that cannot be walked returns
// the zero value and an error, and the caller pushes rather than guessing.
func printDelta(dir string) (deltaPrint, error) {
	var p deltaPrint
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		p.files++
		p.bytes += info.Size()
		if mod := info.ModTime().UnixNano(); mod > p.newest {
			p.newest = mod
		}
		return nil
	})
	return p, err
}

// unchanged reports whether a mount's upper layer is exactly what its last
// successful push carried.
func (r *Runner) unchanged(id string, p deltaPrint) bool {
	r.sync.mu.Lock()
	defer r.sync.mu.Unlock()
	last, ok := r.sync.pushed[id]
	return ok && last == p
}

// recordPush remembers what a successful push (or restore) left in the upper
// layer, so the next tick can tell there is nothing to do. Only successes are
// recorded: a failed push must be retried, and a failed restore must not make
// the next push look unnecessary.
func (r *Runner) recordPush(id string, p deltaPrint) {
	r.sync.mu.Lock()
	defer r.sync.mu.Unlock()
	if r.sync.pushed == nil {
		r.sync.pushed = map[string]deltaPrint{}
	}
	r.sync.pushed[id] = p
}

// restoreDelta seeds a synced mount's upper layer from its last push, before
// the overlay is stacked over it. A destination that does not exist yet is a
// first session, not a failure; anything else fails the mount, because starting
// empty would let the next push overwrite a workspace we simply failed to read.
func (r *Runner) restoreDelta(ctx context.Context, m *artifact.Mount) error {
	upper := UpperDir(filepath.Join(r.sharedVolumePath, m.Out))
	if err := os.MkdirAll(upper, 0o755); err != nil {
		return fmt.Errorf("create upper layer: %w", err)
	}

	archiveRel := deltaArchivePath(m)
	download := &artifact.Download{ID: m.ID + ".restore", In: m.Sync, Out: archiveRel}
	if err := r.apply(ctx, download); err != nil {
		if isNotFound(err) {
			slog.Info("No delta to restore; starting from the image",
				"artifactId", m.ID, "sync", m.Sync)
			return nil
		}
		return fmt.Errorf("restore delta: %w", err)
	}

	unpack := &artifact.Unarchive{
		ID: m.ID + ".unpack",
		In: archiveRel,
		// Relative to the workspace, which is where every artifact path is
		// rooted — the upper layer is an ordinary directory in it.
		Out: relativeTo(r.sharedVolumePath, upper),
	}
	if err := r.apply(ctx, unpack); err != nil {
		return fmt.Errorf("unpack delta: %w", err)
	}
	_ = os.Remove(filepath.Join(r.sharedVolumePath, archiveRel))

	// The restored tree is the baseline: a session that changes nothing then
	// pushes nothing, rather than re-uploading what it just downloaded.
	if p, err := printDelta(upper); err == nil {
		r.recordPush(m.ID, p)
	}
	slog.Info("Restored delta", "artifactId", m.ID, "sync", m.Sync)
	return nil
}

// pushDelta archives the upper layer and uploads it, which is one whole delta
// per push rather than the change since the last one. That is the simple thing
// that is correct; a long session re-uploads a growing archive, and per-file
// sync is the answer when that becomes the cost that matters.
func (r *Runner) pushDelta(ctx context.Context, m *artifact.Mount) error {
	upper := UpperDir(filepath.Join(r.sharedVolumePath, m.Out))
	if _, err := os.Stat(upper); err != nil {
		return nil // nothing mounted, nothing to push
	}

	// Nothing new since the last successful push, so there is nothing to send.
	// This is what makes a short interval affordable: an idle workload costs a
	// directory walk, not an upload. A tree we cannot summarise is pushed rather
	// than assumed unchanged.
	current, printErr := printDelta(upper)
	if printErr == nil && r.unchanged(m.ID, current) {
		return nil
	}

	archiveRel := deltaArchivePath(m)

	pack := &artifact.Archive{
		ID:          m.ID + ".pack",
		In:          relativeTo(r.sharedVolumePath, upper),
		Out:         archiveRel,
		Format:      "tar",
		Compression: "gzip",
	}
	defer func() { _ = os.Remove(filepath.Join(r.sharedVolumePath, archiveRel)) }()
	upload := &artifact.Upload{ID: m.ID + ".push", In: archiveRel, Out: m.Sync}
	if err := r.apply(ctx, pack, upload); err != nil {
		return fmt.Errorf("push delta: %w", err)
	}
	if printErr == nil {
		r.recordPush(m.ID, current)
	}
	return nil
}

// startSync begins pushing a mount's delta on its interval. It runs until
// StopSync, which flushes once more — so a workload that is torn down normally
// loses nothing, and one that dies loses at most an interval.
func (r *Runner) startSync(m *artifact.Mount) {
	interval := time.Duration(m.SyncIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = artifact.DefaultSyncIntervalSeconds * time.Second
	}
	r.sync.mu.Lock()
	defer r.sync.mu.Unlock()
	if r.sync.stop == nil {
		r.sync.stop = make(chan struct{})
	}
	stop := r.sync.stop
	r.sync.mounts = append(r.sync.mounts, m)
	r.sync.done.Go(func() { r.syncLoop(stop, m, interval) })
}

// syncLoop pushes one mount's delta until stop closes. Each push gets the
// interval as its own deadline, so a push that hangs is abandoned rather than
// stacking up behind the next tick.
func (r *Runner) syncLoop(stop <-chan struct{}, m *artifact.Mount, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			err := r.pushDelta(ctx, m)
			cancel()
			if err != nil {
				// The next tick tries again; a push that keeps failing is
				// visible here and costs the caller only the last interval.
				slog.Warn("Delta sync failed", "artifactId", m.ID, "sync", m.Sync, "error", err)
			}
		}
	}
}

// StopSync ends the sync loops and flushes each synced mount once more. Call it
// after the workload has stopped and before unmounting: the delta is read
// through the upper layer, which the unmount takes away.
func (r *Runner) StopSync(ctx context.Context) {
	r.sync.mu.Lock()
	if r.sync.stopped || r.sync.stop == nil {
		r.sync.mu.Unlock()
		return
	}
	r.sync.stopped = true
	stop, mounts := r.sync.stop, r.sync.mounts
	r.sync.mu.Unlock()

	close(stop)
	r.sync.done.Wait()

	for _, m := range mounts {
		if err := r.pushDelta(ctx, m); err != nil {
			slog.Error("Final delta sync failed; work since the last one is lost",
				"artifactId", m.ID, "sync", m.Sync, "error", err)
			continue
		}
		slog.Info("Flushed delta", "artifactId", m.ID, "sync", m.Sync)
	}
}

// apply runs artifacts through the runner's own path, so a sync is the same
// download, archive and upload a caller could have written by hand — including
// the S3 credentials, which stay on this side of the workload.
func (r *Runner) apply(ctx context.Context, arts ...artifact.Artifact) error {
	return r.processArtifacts(ctx, arts, false)
}

// deltaArchivePath is where a delta is staged inside the workspace, next to the
// mount rather than inside it — writing it into the overlay would make the
// archive part of the thing being archived.
func deltaArchivePath(m *artifact.Mount) string {
	return m.Out + ".delta.tgz"
}

// relativeTo expresses path relative to the workspace root, which is what every
// artifact takes.
func relativeTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// isNotFound reports whether an error means the destination has nothing yet — a
// first session — rather than that we could not read it.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "nosuchkey")
}
