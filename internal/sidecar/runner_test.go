package sidecar

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"orchestrator/internal/artifact"
	"orchestrator/pkg/job"
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
	mounted   []string
	unmounted []string
}

func (f *fakeMounter) Mount(image, target string, opts MountOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounted = append(f.mounted, target)
	return nil
}

func (f *fakeMounter) Unmount(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unmounted = append(f.unmounted, target)
	return nil
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

// createTestArchiveFile creates a tar.gz archive file for use in tests.
func createTestArchiveFile(t *testing.T, archivePath string, files map[string]string) {
	t.Helper()

	file, err := os.Create(archivePath)
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
