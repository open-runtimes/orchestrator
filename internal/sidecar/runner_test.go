package sidecar

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// triggerSignal returns a SignalFunc that blocks until trigger is called,
// and a trigger function that unblocks it. Safe to call trigger before Run starts.
func triggerSignal() (SignalFunc, func()) {
	ch := make(chan struct{}, 1)
	fn := func(ctx context.Context) {
		select {
		case <-ctx.Done():
		case <-ch:
		}
	}
	trigger := func() { ch <- struct{}{} }
	return fn, trigger
}

// captureReporter records every artifact report. Thread-safe.
type captureReporter struct {
	mu      sync.Mutex
	reports []job.ArtifactReport
}

func (c *captureReporter) fn() func(job.ArtifactReport) {
	return func(r job.ArtifactReport) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.reports = append(c.reports, r)
	}
}

func (c *captureReporter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reports)
}

func TestCheckReady(t *testing.T) {
	tmpDir := t.TempDir()

	if CheckReady(tmpDir) {
		t.Error("CheckReady should return false when marker doesn't exist")
	}

	markerPath := filepath.Join(tmpDir, ReadyFile)
	if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
		t.Fatalf("Failed to create marker file: %v", err)
	}

	if !CheckReady(tmpDir) {
		t.Error("CheckReady should return true when marker exists")
	}
}

func TestPartition(t *testing.T) {
	artifacts := []artifact.Artifact{
		&artifact.Download{ID: "download", In: "http://example.com/input.tar.gz", Out: "input.tar.gz"},
		&artifact.Unarchive{ID: "extract", In: "input.tar.gz", Out: "code", Depends: "download"},
		&artifact.Archive{ID: "archive", In: "output", Out: "output.tar.gz", Format: "tar", Depends: artifact.JobDependency},
		&artifact.Upload{ID: "upload", In: "output.tar.gz", Out: "http://example.com/upload", Depends: "archive"},
	}

	preJob, postJob := artifact.Partition(artifacts)

	if len(preJob) != 2 {
		t.Errorf("Expected 2 pre-job artifacts, got %d", len(preJob))
	}
	if len(postJob) != 2 {
		t.Errorf("Expected 2 post-job artifacts, got %d", len(postJob))
	}

	preJobIDs := make(map[string]bool)
	for _, a := range preJob {
		preJobIDs[a.ArtifactID()] = true
	}
	if !preJobIDs["download"] || !preJobIDs["extract"] {
		t.Error("Pre-job should contain download and extract")
	}

	postJobIDs := make(map[string]bool)
	for _, a := range postJob {
		postJobIDs[a.ArtifactID()] = true
	}
	if !postJobIDs["archive"] || !postJobIDs["upload"] {
		t.Error("Post-job should contain archive and upload")
	}
}

// TestRunner_FullLifecycle drives the complete sidecar flow:
// pre-job artifacts → ready marker → worker signal → post-job artifacts → reports.
func TestRunner_FullLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	sigFn, triggerDone := triggerSignal()
	captured := &captureReporter{}

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[
		{"id":"pre-write","type":"write","in":"hello","out":"pre.txt"},
		{"id":"post-write","type":"write","in":"world","out":"post.txt","depends":"job"}
	]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 10, reg,
		WithSignalFunc(sigFn),
		WithArtifactListener(captured.fn()),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, artifacts) }()

	// Wait for ready marker — proves pre-job artifacts ran
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if CheckReady(tmpDir) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !CheckReady(tmpDir) {
		t.Fatal("ready marker not written within deadline")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "pre.txt")); err != nil {
		t.Fatal("pre-job artifact file not written")
	}

	// Simulate worker finishing
	triggerDone()

	if err := <-done; err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	// Verify post-job artifact ran
	if _, err := os.Stat(filepath.Join(tmpDir, "post.txt")); err != nil {
		t.Fatal("post-job artifact file not written")
	}

	// Verify reports were sent for both artifacts
	if captured.count() < 2 {
		t.Errorf("expected at least 2 artifact reports, got %d", captured.count())
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	for _, r := range captured.reports {
		if r.Status != "success" {
			t.Errorf("artifact %s: expected status 'success', got %q", r.ID, r.Status)
		}
	}
}

// TestRunner_PostJobMissingFileFailsFast: by the time post-job artifacts run
// the worker has exited, so a source file that isn't there will never appear.
// The artifact must fail within the short flush grace — not park the job (and
// its complete callback) on the full job timeout.
func TestRunner_PostJobMissingFileFailsFast(t *testing.T) {
	tmpDir := t.TempDir()
	sigFn, triggerDone := triggerSignal()
	captured := &captureReporter{}

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[
		{"id":"manifest","type":"read","in":"manifest.json","depends":"job"}
	]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// A 900s job timeout: if the file wait were still bound to it, this test
	// would hang far past its own deadline.
	runner := NewRunner("test-job", tmpDir, 900, reg,
		WithSignalFunc(sigFn),
		WithArtifactListener(captured.fn()),
		WithPostJobFileGrace(100*time.Millisecond),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, artifacts) }()
	triggerDone()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return: post-job file wait is bound to the job timeout")
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	if len(captured.reports) != 1 {
		t.Fatalf("expected 1 artifact report, got %d", len(captured.reports))
	}
	if captured.reports[0].Status != "failed" {
		t.Errorf("expected status 'failed', got %q", captured.reports[0].Status)
	}
}

// TestRunner_PreJobDependencyOrder verifies that pre-job artifacts with a
// depends chain are processed in the correct order.
func TestRunner_PreJobDependencyOrder(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "code.tar.gz")
	createTestArchiveFile(t, archivePath, map[string]string{"main.go": "package main"})

	sigFn, triggerDone := triggerSignal()
	triggerDone() // fire immediately — we only care about pre-job

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[{"id":"extract","type":"unarchive","in":"code.tar.gz","out":"code"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 10, reg, WithSignalFunc(sigFn))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := runner.Run(ctx, artifacts); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "code", "main.go"))
	if err != nil {
		t.Fatalf("extracted file not found: %v", err)
	}
	if string(content) != "package main" {
		t.Errorf("expected 'package main', got %q", string(content))
	}
}

// TestRunner_ChainedDependencies verifies that artifacts with chained depends
// are all processed when running through Run().
func TestRunner_ChainedDependencies(t *testing.T) {
	tmpDir := t.TempDir()
	sigFn, triggerDone := triggerSignal()
	triggerDone()

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[{"id":"file1","type":"write","in":"hello","out":"a.txt"},{"id":"file2","type":"write","in":"world","out":"b.txt","depends":"file1"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 10, reg, WithSignalFunc(sigFn))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := runner.Run(ctx, artifacts); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for _, tc := range []struct{ file, want string }{
		{"a.txt", "hello"},
		{"b.txt", "world"},
	} {
		content, err := os.ReadFile(filepath.Join(tmpDir, tc.file))
		if err != nil {
			t.Fatalf("file %s not found: %v", tc.file, err)
		}
		if string(content) != tc.want {
			t.Errorf("file %s: expected %q, got %q", tc.file, tc.want, string(content))
		}
	}
}

// TestRunner_CircularDependency verifies that Run() completes without hanging
// when pre-job artifacts have a circular dependency.
func TestRunner_CircularDependency(t *testing.T) {
	tmpDir := t.TempDir()
	sigFn, triggerDone := triggerSignal()
	triggerDone()

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[{"id":"a","type":"write","in":"a","out":"a.txt","depends":"b"},{"id":"b","type":"write","in":"b","out":"b.txt","depends":"a"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 5, reg, WithSignalFunc(sigFn))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		runner.Run(ctx, artifacts)
		close(done)
	}()

	select {
	case <-done:
		// good — completed without hanging
	case <-time.After(4 * time.Second):
		t.Error("Run() hung on circular dependency")
	}
}

// TestRunner_ReportsArtifact verifies that artifact results are reported with the correct fields.
func TestRunner_ReportsArtifact(t *testing.T) {
	tmpDir := t.TempDir()
	sigFn, triggerDone := triggerSignal()
	triggerDone()

	captured := &captureReporter{}

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[{"id":"w","type":"write","in":"data","out":"out.txt"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 5, reg,
		WithSignalFunc(sigFn),
		WithArtifactListener(captured.fn()),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := runner.Run(ctx, artifacts); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if captured.count() == 0 {
		t.Fatal("no artifact reports captured")
	}

	captured.mu.Lock()
	defer captured.mu.Unlock()
	r := captured.reports[0]
	if r.ID != "w" {
		t.Errorf("expected ID 'w', got %q", r.ID)
	}
	if r.Type != "write" {
		t.Errorf("expected Type 'write', got %q", r.Type)
	}
	if r.Status != "success" {
		t.Errorf("expected Status 'success', got %q", r.Status)
	}
}

// fakeMounter records Mount/Unmount calls without touching the kernel.
type fakeMounter struct {
	mu        sync.Mutex
	sources   []string
	opts      []MountOpts
	mounted   []string
	unmounted []string
	active    map[string]bool
}

func (f *fakeMounter) Mount(image, target string, opts MountOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources = append(f.sources, image)
	f.opts = append(f.opts, opts)
	f.mounted = append(f.mounted, target)
	if f.active == nil {
		f.active = map[string]bool{}
	}
	f.active[target] = true
	return nil
}

func (f *fakeMounter) Unmount(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounted = append(f.unmounted, target)
	if f.active != nil {
		f.active[target] = false
	}
	return nil
}

func (f *fakeMounter) IsMounted(target string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active != nil && f.active[target], nil
}

// TestRunner_MountLifecycle verifies the post sidecar mounts before signaling
// the mounts-ready marker (which gates the worker) and unmounts on teardown.
func TestRunner_MountLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "data.sqfs"), []byte("hsqs"), 0o644); err != nil {
		t.Fatal(err)
	}

	sigFn, triggerDone := triggerSignal()
	fake := &fakeMounter{}
	target := filepath.Join(tmpDir, "mnt", "data")

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[{"id":"m","type":"mount","in":"data.sqfs","out":"mnt/data"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 10, reg, WithSignalFunc(sigFn), WithMounter(fake))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runner.RunPost(ctx, artifacts) }()

	// The mounts-ready marker proves the mount completed before the worker gate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !CheckMountsReady(tmpDir) {
		time.Sleep(20 * time.Millisecond)
	}
	if !CheckMountsReady(tmpDir) {
		t.Fatal("mounts-ready marker not written within deadline")
	}

	fake.mu.Lock()
	if len(fake.mounted) != 1 || fake.mounted[0] != target {
		t.Fatalf("expected mount of %q, got %v", target, fake.mounted)
	}
	if len(fake.unmounted) != 0 {
		t.Fatalf("should not unmount before worker finishes, got %v", fake.unmounted)
	}
	fake.mu.Unlock()

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("mount target dir not created: %v", err)
	}

	triggerDone() // worker finished

	if err := <-done; err != nil {
		t.Fatalf("RunPost() error = %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.unmounted) != 1 || fake.unmounted[0] != target {
		t.Fatalf("expected unmount of %q on teardown, got %v", target, fake.unmounted)
	}
}

func TestRunner_RunPostAdoptsMountAfterSidecarRestart(t *testing.T) {
	workspace := t.TempDir()
	createTarFile(t, filepath.Join(workspace, "data.tar.gz"), true, map[string]string{"index.js": "export default {}"})
	target := filepath.Join(workspace, "runtime")
	if err := os.MkdirAll(filepath.Join(target, ".lower"), 0o755); err != nil {
		t.Fatal(err)
	}

	fake := &fakeMounter{active: map[string]bool{target: true}}
	sigFn, triggerDone := triggerSignal()
	runner := NewRunner("test-job", workspace, 10, artifact.DefaultRegistry(), WithSignalFunc(sigFn), WithMounter(fake))
	artifacts := []artifact.Artifact{&artifact.Mount{ID: "m", In: "data.tar.gz", Out: "runtime", Writable: true}}

	done := make(chan error, 1)
	go func() { done <- runner.RunPost(t.Context(), artifacts) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !CheckMountsReady(workspace) {
		time.Sleep(20 * time.Millisecond)
	}
	if !CheckMountsReady(workspace) {
		t.Fatal("restarted sidecar did not restore the mounts-ready marker")
	}
	fake.mu.Lock()
	if len(fake.mounted) != 0 {
		t.Fatalf("existing mount was re-established: %v", fake.mounted)
	}
	fake.mu.Unlock()

	triggerDone()
	if err := <-done; err != nil {
		t.Fatalf("RunPost() error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.unmounted) != 1 || fake.unmounted[0] != target {
		t.Fatalf("adopted mount was not cleaned up: %v", fake.unmounted)
	}
}

func TestRunner_TarMountMaterializesDirectoryLower(t *testing.T) {
	for _, name := range []string{"data.tar", "data.tar.gz"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			archivePath := filepath.Join(workspace, name)
			createTarFile(t, archivePath, name == "data.tar.gz", map[string]string{
				"bin/start": "#!/bin/sh\necho ready\n",
				"notes.txt": "change me",
			})

			fake := &fakeMounter{}
			runner := NewRunner("test-job", workspace, 10, artifact.DefaultRegistry(), WithMounter(fake))
			mount := &artifact.Mount{ID: "m", In: name, Out: "runtime", Writable: true}
			staleLower := filepath.Join(workspace, "runtime", ".lower")
			if err := os.MkdirAll(staleLower, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(staleLower, "stale"), []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := runner.Mount(t.Context(), []artifact.Artifact{mount}); err != nil {
				t.Fatalf("Mount() error = %v", err)
			}
			defer runner.Release()

			lower := filepath.Join(workspace, "runtime", ".lower")
			if _, err := os.Stat(filepath.Join(lower, "stale")); !os.IsNotExist(err) {
				t.Fatalf("stale lower entry survived re-extraction: %v", err)
			}
			content, err := os.ReadFile(filepath.Join(lower, "bin", "start"))
			if err != nil {
				t.Fatalf("read extracted lower: %v", err)
			}
			if string(content) != "#!/bin/sh\necho ready\n" {
				t.Fatalf("extracted content = %q", content)
			}
			for path, want := range map[string]os.FileMode{
				filepath.Join(lower, "bin"):       0o777,
				filepath.Join(lower, "bin/start"): 0o666,
				filepath.Join(lower, "notes.txt"): 0o666,
			} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat writable lower %s: %v", path, err)
				}
				if got := info.Mode().Perm(); got != want {
					t.Errorf("writable lower %s mode = %o, want %o", path, got, want)
				}
			}

			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.sources) != 1 || fake.sources[0] != lower {
				t.Fatalf("mount source = %v, want [%s]", fake.sources, lower)
			}
			if len(fake.opts) != 1 || !fake.opts[0].SourceDir || !fake.opts[0].Writable {
				t.Fatalf("mount opts = %+v, want directory + writable", fake.opts)
			}
		})
	}
}

func TestRunner_InvalidTarMountRemovesPartialLower(t *testing.T) {
	workspace := t.TempDir()
	archivePath := filepath.Join(workspace, "broken.tar.gz")
	if err := os.WriteFile(archivePath, append([]byte{0x1f, 0x8b}, []byte("not gzip")...), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeMounter{}
	runner := NewRunner("test-job", workspace, 10, artifact.DefaultRegistry(), WithMounter(fake))
	err := runner.Mount(t.Context(), []artifact.Artifact{&artifact.Mount{ID: "m", In: "broken.tar.gz", Out: "runtime"}})
	if err == nil {
		t.Fatal("Mount() error = nil, want invalid archive error")
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "runtime", ".lower")); !os.IsNotExist(statErr) {
		t.Fatalf("partial lower remains after failure: %v", statErr)
	}
	if len(fake.mounted) != 0 {
		t.Fatalf("mounter called for invalid tar: %v", fake.mounted)
	}
}

// createTestArchiveFile creates a tar.gz archive file for use in tests.
func createTestArchiveFile(t *testing.T, archivePath string, files map[string]string) {
	t.Helper()
	createTarFile(t, archivePath, true, files)
}

func createTarFile(t *testing.T, archivePath string, compressed bool, files map[string]string) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Failed to create archive file: %v", err)
	}
	defer file.Close()

	var output io.Writer = file
	var gzWriter *gzip.Writer
	if compressed {
		gzWriter = gzip.NewWriter(file)
		output = gzWriter
		defer gzWriter.Close()
	}
	tarWriter := tar.NewWriter(output)
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

func TestNewHTTPSink_SendsBearerToken(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	sink := NewHTTPSink("job-1", srv.URL, "tok-1", time.Second, "", "", nil, nil)
	sink(job.ArtifactReport{ID: "a1", Status: "success"})

	if gotAuth != "Bearer tok-1" {
		t.Errorf("Authorization: want %q, got %q", "Bearer tok-1", gotAuth)
	}
}

func TestWaitForPath_ExistingFileReturnsBeforeFirstTick(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "already-there")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	r := NewRunner("test-job", tmpDir, 10, artifact.DefaultRegistry())

	start := time.Now()
	if err := r.waitForPath(t.Context(), path); err != nil {
		t.Fatalf("waitForPath: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Errorf("waitForPath waited a full tick (%v) for a file that already existed", elapsed)
	}
}

// TestRunner_HoldUntilShutdownLifecycle verifies the single-sidecar flow for
// long-lived pods: downloads and mounts land before the ready marker (which
// gates the app container), a total-duration report is emitted, and pod
// shutdown tears the mounts down.
func TestRunner_HoldUntilShutdownLifecycle(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "data.sqfs"), []byte("hsqs"), 0o644); err != nil {
		t.Fatal(err)
	}

	sigFn, triggerDone := triggerSignal()
	fake := &fakeMounter{}
	captured := &captureReporter{}
	target := filepath.Join(tmpDir, "mnt", "data")

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[
		{"id":"seed","type":"write","in":"hello","out":"pre.txt"},
		{"id":"m","type":"mount","in":"data.sqfs","out":"mnt/data"}
	]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// timeoutSeconds 0: an unbounded workload that holds until pod shutdown.
	runner := NewRunner("test-job", tmpDir, 0, reg,
		WithSignalFunc(sigFn),
		WithMounter(fake),
		WithArtifactListener(captured.fn()),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx, artifacts) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !CheckReady(tmpDir) {
		time.Sleep(20 * time.Millisecond)
	}
	if !CheckReady(tmpDir) {
		t.Fatal("ready marker not written within deadline")
	}

	fake.mu.Lock()
	if len(fake.mounted) != 1 || fake.mounted[0] != target {
		t.Fatalf("expected mount of %q before ready marker, got %v", target, fake.mounted)
	}
	if len(fake.unmounted) != 0 {
		t.Fatalf("should not unmount while runtime is live, got %v", fake.unmounted)
	}
	fake.mu.Unlock()

	triggerDone() // pod shutdown

	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.unmounted) != 1 || fake.unmounted[0] != target {
		t.Fatalf("expected unmount of %q on shutdown, got %v", target, fake.unmounted)
	}
}

// TestRunner_HoldDeadline pins what TIMEOUT_SECONDS controls: a bounded job's
// wait carries the job deadline (it is what unsticks a sidecar whose signal
// never arrives), while an unbounded workload's (timeout 0) must not — the
// deadline expiring would tear the code mount out from under a serving app.
func TestRunner_HoldDeadline(t *testing.T) {
	for _, tc := range []struct {
		name           string
		timeoutSeconds int
		wantDeadline   bool
	}{
		{"bounded job wait carries the deadline", 1, true},
		{"unbounded workload wait has none", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			deadlineSet := make(chan bool, 1)
			waitFn := func(ctx context.Context) {
				_, ok := ctx.Deadline()
				deadlineSet <- ok
			}

			reg := artifact.DefaultRegistry()
			artifacts, err := reg.Unmarshal([]byte(`[{"id":"seed","type":"write","in":"hello","out":"pre.txt"}]`))
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			runner := NewRunner("test-job", tmpDir, tc.timeoutSeconds, reg, WithSignalFunc(waitFn), WithMounter(&fakeMounter{}))
			if err := runner.Run(t.Context(), artifacts); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if got := <-deadlineSet; got != tc.wantDeadline {
				t.Fatalf("hold context deadline = %v, want %v", got, tc.wantDeadline)
			}
		})
	}
}

// TestRunner_RestartAdoptsMounts verifies a restarted combined sidecar
// recovers in place: the surviving mount is adopted rather than re-established,
// the stale ready marker is replaced only after adoption, and shutdown still
// unmounts the adopted target.
func TestRunner_RestartAdoptsMounts(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "data.sqfs"), []byte("hsqs"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Debris from the previous incarnation: ready marker and completed download.
	if err := os.WriteFile(filepath.Join(tmpDir, ReadyFile), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	sigFn, triggerDone := triggerSignal()
	target := filepath.Join(tmpDir, "mnt", "data")
	fake := &fakeMounter{active: map[string]bool{target: true}}

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(`[{"id":"m","type":"mount","in":"data.sqfs","out":"mnt/data"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	runner := NewRunner("test-job", tmpDir, 0, reg, WithSignalFunc(sigFn), WithMounter(fake))

	done := make(chan error, 1)
	go func() { done <- runner.Run(t.Context(), artifacts) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !CheckReady(tmpDir) {
		time.Sleep(20 * time.Millisecond)
	}
	if !CheckReady(tmpDir) {
		t.Fatal("ready marker not rewritten within deadline")
	}

	fake.mu.Lock()
	if len(fake.mounted) != 0 {
		t.Fatalf("adopted mount must not be re-established, got Mount calls for %v", fake.mounted)
	}
	fake.mu.Unlock()

	triggerDone()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.unmounted) != 1 || fake.unmounted[0] != target {
		t.Fatalf("expected adopted mount %q unmounted on shutdown, got %v", target, fake.unmounted)
	}
}
