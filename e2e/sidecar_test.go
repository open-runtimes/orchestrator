//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/sidecar"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// TestSidecar_FullFlow tests the sidecar's artifact handling.
// Note: start/exit events are now handled by the Docker orchestrator, not the sidecar.
// The sidecar only handles artifact processing (downloads, uploads, etc.).
func TestSidecar_FullFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	sharedDir, err := os.MkdirTemp("", "sidecar-test-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(sharedDir)

	var eventCount atomic.Int64
	var mu sync.Mutex
	receivedEvents := make([]map[string]any, 0)

	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Logf("Failed to decode callback: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mu.Lock()
		receivedEvents = append(receivedEvents, event)
		if eventType, ok := event["type"].(string); ok {
			t.Logf("Received callback: %s", eventType)
		}
		mu.Unlock()
		eventCount.Add(1)

		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	reader, err := dockerClient.ImagePull(ctx, "alpine:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("Failed to pull image: %v", err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	jobID := fmt.Sprintf("sidecar-test-%d", time.Now().UnixNano())
	containerName := fmt.Sprintf("job-%s-worker", jobID)

	containerConfig := &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"/bin/sh", "-c", "echo 'hello from job' > /workspace/output.txt && sleep 1"},
	}

	hostConfig := &container.HostConfig{
		Binds: []string{
			fmt.Sprintf("%s:/workspace", sharedDir),
		},
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}

	defer func() {
		timeout := 5
		_ = dockerClient.ContainerStop(ctx, resp.ID, container.StopOptions{Timeout: &timeout})
		_ = dockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	}()

	if err := dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	cfg := &sidecar.Config{
		JobID:            jobID,
		CallbackURL:      callbackServer.URL,
		CallbackEvents:   "orchestrator.job.artifact",
		TimeoutSeconds:   60,
		SharedVolumePath: sharedDir,
		ArtifactsJSON:    `[{"id":"result","type":"read","in":"output.txt","depends":"job"}]`,
	}

	runner, err := sidecar.NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create sidecar runner: %v", err)
	}
	defer runner.Close()

	// Run sidecar in goroutine since it will block waiting for signal
	sidecarDone := make(chan error, 1)
	go func() {
		sidecarDone <- runner.Run(ctx)
	}()

	// Wait for container to exit, then signal sidecar
	statusCh, errCh := dockerClient.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-statusCh:
	case err := <-errCh:
		t.Fatalf("Error waiting for container: %v", err)
	}

	// Signal sidecar that worker is done
	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)

	// Wait for sidecar to finish
	if err := <-sidecarDone; err != nil {
		t.Errorf("Sidecar run failed: %v", err)
	}

	// Wait for at least one callback event
	testutil.MustWaitForCount(t, &eventCount, 1, testutil.WithTimeout(10*time.Second))

	mu.Lock()
	defer mu.Unlock()

	if len(receivedEvents) == 0 {
		t.Error("No callback events received")
	}

	// Verify we received the artifact event
	hasArtifactEvent := false
	for _, event := range receivedEvents {
		if event["type"] == "orchestrator.job.artifact" {
			hasArtifactEvent = true
			break
		}
	}

	if !hasArtifactEvent {
		t.Error("Missing expected artifact event")
	}

	t.Logf("Received %d callback events", len(receivedEvents))
}

func TestSidecar_InputDownload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	sharedDir, err := os.MkdirTemp("", "sidecar-input-test-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(sharedDir)

	inputContent := "downloaded input content"
	inputServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(inputContent))
	}))
	defer inputServer.Close()

	var mu sync.Mutex
	receivedEvents := make([]map[string]any, 0)

	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		receivedEvents = append(receivedEvents, event)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	reader, err := dockerClient.ImagePull(ctx, "alpine:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("Failed to pull image: %v", err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	jobID := fmt.Sprintf("sidecar-input-%d", time.Now().UnixNano())
	containerName := fmt.Sprintf("job-%s-worker", jobID)

	containerConfig := &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"/bin/sh", "-c", "sleep 2 && cat /workspace/input.txt"},
	}

	hostConfig := &container.HostConfig{
		Binds: []string{
			fmt.Sprintf("%s:/workspace", sharedDir),
		},
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}

	defer func() {
		timeout := 5
		_ = dockerClient.ContainerStop(ctx, resp.ID, container.StopOptions{Timeout: &timeout})
		_ = dockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	}()

	if err := dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	artifactsJSON := fmt.Sprintf(`[{"id":"input-1","type":"download","out":"input.txt","in":"%s"}]`, inputServer.URL)
	cfg := &sidecar.Config{
		JobID:            jobID,
		ArtifactsJSON:    artifactsJSON,
		CallbackURL:      callbackServer.URL,
		CallbackEvents:   "orchestrator.job.artifact",
		TimeoutSeconds:   60,
		SharedVolumePath: sharedDir,
	}

	runner, err := sidecar.NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create sidecar runner: %v", err)
	}
	defer runner.Close()

	// Run sidecar in goroutine since it will block waiting for signal
	sidecarDone := make(chan error, 1)
	go func() {
		sidecarDone <- runner.Run(ctx)
	}()

	// Wait for ready marker (pre-job artifacts complete)
	readyPath := filepath.Join(sharedDir, sidecar.ReadyFile)
	testutil.MustWaitFor(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, testutil.WithTimeout(10*time.Second))

	// Signal sidecar that worker is done (no actual worker in this test)
	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)

	// Wait for sidecar to finish
	if err := <-sidecarDone; err != nil {
		t.Errorf("Sidecar run failed: %v", err)
	}

	downloadedPath := filepath.Join(sharedDir, "input.txt")
	content, err := os.ReadFile(downloadedPath)
	if err != nil {
		t.Errorf("Failed to read downloaded input: %v", err)
	} else if string(content) != inputContent {
		t.Errorf("Downloaded content mismatch: got %q, want %q", string(content), inputContent)
	}

	mu.Lock()
	defer mu.Unlock()

	hasArtifactEvent := false
	for _, event := range receivedEvents {
		if event["type"] == "orchestrator.job.artifact" {
			hasArtifactEvent = true
			if data, ok := event["data"].(map[string]any); ok {
				if data["status"] != "success" {
					t.Errorf("Artifact event status: got %v, want 'success'", data["status"])
				}
			}
			break
		}
	}

	if !hasArtifactEvent {
		t.Error("No artifact event received")
	}
}

func TestSidecar_OutputUpload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	sharedDir, err := os.MkdirTemp("", "sidecar-output-test-")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(sharedDir)

	var uploadedContent []byte
	var uploadMu sync.Mutex
	var uploadCount atomic.Int64

	uploadServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			uploadMu.Lock()
			uploadedContent, _ = io.ReadAll(r.Body)
			uploadMu.Unlock()
			uploadCount.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer uploadServer.Close()

	var eventCount atomic.Int64
	var mu sync.Mutex
	receivedEvents := make([]map[string]any, 0)

	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event map[string]any
		json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		receivedEvents = append(receivedEvents, event)
		mu.Unlock()
		eventCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer callbackServer.Close()

	reader, err := dockerClient.ImagePull(ctx, "alpine:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("Failed to pull image: %v", err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	jobID := fmt.Sprintf("sidecar-output-%d", time.Now().UnixNano())
	containerName := fmt.Sprintf("job-%s-worker", jobID)

	outputContent := "this is the job output"
	containerConfig := &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"/bin/sh", "-c", fmt.Sprintf("echo -n '%s' > /workspace/result.txt", outputContent)},
	}

	hostConfig := &container.HostConfig{
		Binds: []string{
			fmt.Sprintf("%s:/workspace", sharedDir),
		},
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, containerName)
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}

	defer func() {
		timeout := 5
		_ = dockerClient.ContainerStop(ctx, resp.ID, container.StopOptions{Timeout: &timeout})
		_ = dockerClient.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
	}()

	if err := dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	artifactsJSON := fmt.Sprintf(`[{"id":"result","type":"upload","in":"result.txt","out":"%s","depends":"job"}]`, uploadServer.URL)
	cfg := &sidecar.Config{
		JobID:            jobID,
		CallbackURL:      callbackServer.URL,
		CallbackEvents:   "orchestrator.job.artifact",
		TimeoutSeconds:   60,
		SharedVolumePath: sharedDir,
		ArtifactsJSON:    artifactsJSON,
	}

	runner, err := sidecar.NewRunner(cfg)
	if err != nil {
		t.Fatalf("Failed to create sidecar runner: %v", err)
	}
	defer runner.Close()

	// Run sidecar in goroutine since it will block waiting for signal
	sidecarDone := make(chan error, 1)
	go func() {
		sidecarDone <- runner.Run(ctx)
	}()

	// Wait for container to exit, then signal sidecar
	statusCh, errCh := dockerClient.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-statusCh:
	case err := <-errCh:
		t.Fatalf("Error waiting for container: %v", err)
	}

	// Signal sidecar that worker is done
	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)

	// Wait for sidecar to finish
	if err := <-sidecarDone; err != nil {
		t.Errorf("Sidecar run failed: %v", err)
	}

	// Wait for upload and callback
	testutil.MustWaitForCount(t, &uploadCount, 1, testutil.WithTimeout(10*time.Second))
	testutil.MustWaitForCount(t, &eventCount, 1, testutil.WithTimeout(10*time.Second))

	uploadMu.Lock()
	if string(uploadedContent) != outputContent {
		t.Errorf("Uploaded content mismatch: got %q, want %q", string(uploadedContent), outputContent)
	}
	uploadMu.Unlock()

	mu.Lock()
	defer mu.Unlock()

	hasArtifactEvent := false
	for _, event := range receivedEvents {
		if event["type"] == "orchestrator.job.artifact" {
			hasArtifactEvent = true
			if data, ok := event["data"].(map[string]any); ok {
				if data["status"] != "success" {
					t.Errorf("Artifact event status: got %v, want 'success'", data["status"])
				}
			}
			break
		}
	}

	if !hasArtifactEvent {
		t.Error("No artifact event received")
	}
}

// Note: Job failure (exit code) monitoring is now handled by the Docker orchestrator,
// not the sidecar. See internal/docker/docker_integration_test.go for exit code tests.
