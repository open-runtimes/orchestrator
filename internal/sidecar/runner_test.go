package sidecar

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"orchestrator/internal/artifact"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckReady(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "check-ready-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Initially should return false (no marker file)
	if CheckReady(tmpDir) {
		t.Error("CheckReady should return false when marker doesn't exist")
	}

	// Create marker file
	markerPath := filepath.Join(tmpDir, ReadyFile)
	if err := os.WriteFile(markerPath, []byte{}, 0o644); err != nil {
		t.Fatalf("Failed to create marker file: %v", err)
	}

	// Now should return true
	if !CheckReady(tmpDir) {
		t.Error("CheckReady should return true when marker exists")
	}
}

func TestRunner_WritesReadyMarker(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-marker-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &Config{
		JobID:            "test-job",
		TimeoutSeconds:   5,
		SharedVolumePath: tmpDir,
	}

	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create runner: %v", err)
	}
	defer runner.Close()

	// Run in background (will block waiting for signal)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.Run(ctx)
	}()

	// Wait for marker file to appear
	markerPath := filepath.Join(tmpDir, ReadyFile)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(markerPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify marker exists
	if _, err := os.Stat(markerPath); os.IsNotExist(err) {
		t.Error("Runner should write ready marker file")
	}

	// Verify CheckReady works with it
	if !CheckReady(tmpDir) {
		t.Error("CheckReady should return true after runner writes marker")
	}

	// Cancel to stop the runner
	cancel()
	<-done
}

func TestSeparateArtifacts(t *testing.T) {
	artifacts := []artifact.Artifact{
		&artifact.Download{ID: "download", In: "http://example.com/input.tar.gz", Out: "input.tar.gz"},
		&artifact.Unarchive{ID: "extract", In: "input.tar.gz", Out: "code", Depends: "download"},
		&artifact.Archive{ID: "archive", In: "output", Out: "output.tar.gz", Format: "tar.gz", Depends: artifact.JobDependency},
		&artifact.Upload{ID: "upload", In: "output.tar.gz", Out: "http://example.com/upload", Depends: "archive"},
	}

	preJob, postJob := separateArtifacts(artifacts)

	if len(preJob) != 2 {
		t.Errorf("Expected 2 pre-job artifacts, got %d", len(preJob))
	}
	if len(postJob) != 2 {
		t.Errorf("Expected 2 post-job artifacts, got %d", len(postJob))
	}

	// Verify pre-job artifacts
	preJobIDs := make(map[string]bool)
	for _, a := range preJob {
		preJobIDs[a.ArtifactID()] = true
	}
	if !preJobIDs["download"] || !preJobIDs["extract"] {
		t.Error("Pre-job should contain download and extract")
	}

	// Verify post-job artifacts
	postJobIDs := make(map[string]bool)
	for _, a := range postJob {
		postJobIDs[a.ArtifactID()] = true
	}
	if !postJobIDs["archive"] || !postJobIDs["upload"] {
		t.Error("Post-job should contain archive and upload")
	}
}

func TestRunner_ArtifactDependencyOrder(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-artifact-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test archive file directly
	archivePath := filepath.Join(tmpDir, "code.tar.gz")
	createTestArchiveFile(t, archivePath, map[string]string{
		"main.go": "package main",
	})

	cfg := &Config{
		JobID:            "test-job",
		TimeoutSeconds:   30,
		SharedVolumePath: tmpDir,
		ArtifactsJSON:    `[{"id":"extract","type":"unarchive","in":"code.tar.gz","out":"code"}]`,
	}

	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Process pre-job artifacts
	err = runner.processArtifacts(ctx, runner.preJobArtifacts, false)
	if err != nil {
		t.Fatalf("processArtifacts() error = %v", err)
	}

	// Verify extracted file exists
	extractedPath := filepath.Join(tmpDir, "code", "main.go")
	content, err := os.ReadFile(extractedPath)
	if err != nil {
		t.Fatalf("Failed to read extracted file: %v", err)
	}
	if string(content) != "package main" {
		t.Errorf("Expected 'package main', got %q", string(content))
	}
}

func TestRunner_ArtifactChainedDependencies(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-artifact-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &Config{
		JobID:            "test-job",
		TimeoutSeconds:   30,
		SharedVolumePath: tmpDir,
		ArtifactsJSON:    `[{"id":"file1","type":"write","in":"hello","out":"a.txt"},{"id":"file2","type":"write","in":"world","out":"b.txt","depends":"file1"}]`,
	}

	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Process pre-job artifacts
	err = runner.processArtifacts(ctx, runner.preJobArtifacts, false)
	if err != nil {
		t.Fatalf("processArtifacts() error = %v", err)
	}

	// Verify both files exist
	content1, err := os.ReadFile(filepath.Join(tmpDir, "a.txt"))
	if err != nil {
		t.Fatalf("Failed to read a.txt: %v", err)
	}
	if string(content1) != "hello" {
		t.Errorf("Expected 'hello', got %q", string(content1))
	}

	content2, err := os.ReadFile(filepath.Join(tmpDir, "b.txt"))
	if err != nil {
		t.Fatalf("Failed to read b.txt: %v", err)
	}
	if string(content2) != "world" {
		t.Errorf("Expected 'world', got %q", string(content2))
	}
}

func TestRunner_ArtifactCircularDependency(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "runner-artifact-test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := &Config{
		JobID:            "test-job",
		TimeoutSeconds:   30,
		SharedVolumePath: tmpDir,
		ArtifactsJSON:    `[{"id":"a","type":"write","in":"a","out":"a.txt","depends":"b"},{"id":"b","type":"write","in":"b","out":"b.txt","depends":"a"}]`,
	}

	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create runner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Should not hang - circular dependencies are detected and skipped
	done := make(chan struct{})
	go func() {
		runner.processArtifacts(ctx, runner.preJobArtifacts, false)
		close(done)
	}()

	select {
	case <-done:
		// Good - completed without hanging
	case <-time.After(3 * time.Second):
		t.Error("processArtifacts hung on circular dependency")
	}
}

// createTestArchiveFile creates a tar.gz archive file
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
