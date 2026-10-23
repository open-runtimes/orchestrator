//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/job"
	"orchestrator/internal/sidecar"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// mockOrchestratorServer returns a test server that captures ArtifactReports posted
// by the sidecar to /internal/jobs/{jobId}/artifact.
func mockOrchestratorServer(t *testing.T, count *atomic.Int64, mu *sync.Mutex, reports *[]job.ArtifactReport) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report job.ArtifactReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Logf("Failed to decode artifact report: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		t.Logf("Received artifact report: id=%s type=%s status=%s", report.ID, report.Type, report.Status)
		mu.Lock()
		*reports = append(*reports, report)
		mu.Unlock()
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
}

// TestSidecar_FullFlow tests that the sidecar processes a post-job artifact and
// posts an ArtifactReport to the orchestrator endpoint with the correct fields,
// including callback config that the orchestrator will use for dispatch.
func TestSidecar_FullFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	sharedDir := t.TempDir()

	var reportCount atomic.Int64
	var mu sync.Mutex
	var receivedReports []job.ArtifactReport
	orchestratorServer := mockOrchestratorServer(t, &reportCount, &mu, &receivedReports)
	defer orchestratorServer.Close()

	reader, err := dockerClient.ImagePull(ctx, "alpine:latest", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("Failed to pull image: %v", err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	jobID := fmt.Sprintf("sidecar-test-%d", time.Now().UnixNano())
	containerName := fmt.Sprintf("job-%s-worker", jobID)

	resp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"/bin/sh", "-c", "echo 'hello from job' > /workspace/output.txt && sleep 1"},
		},
		HostConfig: &container.HostConfig{
			Binds: []string{sharedDir + ":/workspace"},
		},
		Name: containerName,
	})
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}
	defer func() {
		timeout := 5
		_, _ = dockerClient.ContainerStop(ctx, resp.ID, client.ContainerStopOptions{Timeout: &timeout})
		_, _ = dockerClient.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	artifacts, err := artifact.UnmarshalArtifacts([]byte(`[{"id":"result","type":"read","in":"output.txt","depends":"job"}]`))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	reporter := sidecar.NewHTTPSink(jobID, orchestratorServer.URL, "", 30*time.Second,
		"http://example.com/callback", "", []string{"orchestrator.job.artifact"}, nil)

	runner := sidecar.NewRunner(jobID, sharedDir, 60,
		sidecar.WithArtifactListener(reporter),
	)

	sidecarDone := make(chan error, 1)
	go func() { sidecarDone <- runner.Run(ctx, artifacts) }()

	wait := dockerClient.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	statusCh, errCh := wait.Result, wait.Error
	select {
	case <-statusCh:
	case err := <-errCh:
		t.Fatalf("Error waiting for container: %v", err)
	}

	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)

	if err := <-sidecarDone; err != nil {
		t.Errorf("Sidecar run failed: %v", err)
	}

	testutil.MustWaitForCount(t, &reportCount, 1, testutil.WithTimeout(10*time.Second))

	mu.Lock()
	defer mu.Unlock()

	if len(receivedReports) == 0 {
		t.Fatal("No artifact reports received")
	}
	r := receivedReports[0]
	if r.ID != "result" {
		t.Errorf("ArtifactID: got %q, want %q", r.ID, "result")
	}
	if r.Status != "success" {
		t.Errorf("Status: got %q, want %q", r.Status, "success")
	}
	if r.CallbackURL != "http://example.com/callback" {
		t.Errorf("CallbackURL: got %q, want %q", r.CallbackURL, "http://example.com/callback")
	}
	if len(r.CallbackEvents) != 1 || r.CallbackEvents[0] != "orchestrator.job.artifact" {
		t.Errorf("CallbackEvents: got %v, want [orchestrator.job.artifact]", r.CallbackEvents)
	}
}

// TestSidecar_InputDownload tests that the sidecar downloads a pre-job artifact
// and reports the result to the orchestrator endpoint.
func TestSidecar_InputDownload(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	sharedDir := t.TempDir()

	inputContent := "downloaded input content"
	inputServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(inputContent))
	}))
	defer inputServer.Close()

	var reportCount atomic.Int64
	var mu sync.Mutex
	var receivedReports []job.ArtifactReport
	orchestratorServer := mockOrchestratorServer(t, &reportCount, &mu, &receivedReports)
	defer orchestratorServer.Close()

	reader, err := dockerClient.ImagePull(ctx, "alpine:latest", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("Failed to pull image: %v", err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	jobID := fmt.Sprintf("sidecar-input-%d", time.Now().UnixNano())
	containerName := fmt.Sprintf("job-%s-worker", jobID)

	resp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"/bin/sh", "-c", "sleep 2 && cat /workspace/input.txt"},
		},
		HostConfig: &container.HostConfig{
			Binds: []string{sharedDir + ":/workspace"},
		},
		Name: containerName,
	})
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}
	defer func() {
		timeout := 5
		_, _ = dockerClient.ContainerStop(ctx, resp.ID, client.ContainerStopOptions{Timeout: &timeout})
		_, _ = dockerClient.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	artifacts, err := artifact.UnmarshalArtifacts(fmt.Appendf(nil, `[{"id":"input-1","type":"download","out":"input.txt","in":"%s"}]`, inputServer.URL))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	reporter := sidecar.NewHTTPSink(jobID, orchestratorServer.URL, "", 30*time.Second, "", "", nil, nil)

	runner := sidecar.NewRunner(jobID, sharedDir, 60,
		sidecar.WithArtifactListener(reporter),
	)

	sidecarDone := make(chan error, 1)
	go func() { sidecarDone <- runner.Run(ctx, artifacts) }()

	// Wait for pre-job artifacts to complete (ready marker written)
	readyPath := sidecar.ReadyMarkerPath(sharedDir)
	testutil.MustWaitFor(t, func() bool {
		_, err := os.Stat(readyPath)
		return err == nil
	}, testutil.WithTimeout(10*time.Second))

	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)

	if err := <-sidecarDone; err != nil {
		t.Errorf("Sidecar run failed: %v", err)
	}

	// Verify the file was downloaded
	content, err := os.ReadFile(filepath.Join(sharedDir, "input.txt"))
	if err != nil {
		t.Errorf("Failed to read downloaded input: %v", err)
	} else if string(content) != inputContent {
		t.Errorf("Downloaded content mismatch: got %q, want %q", string(content), inputContent)
	}

	// Verify the artifact report was sent
	testutil.MustWaitForCount(t, &reportCount, 1, testutil.WithTimeout(10*time.Second))

	mu.Lock()
	defer mu.Unlock()

	if len(receivedReports) == 0 {
		t.Fatal("No artifact reports received")
	}
	r := receivedReports[0]
	if r.ID != "input-1" {
		t.Errorf("ArtifactID: got %q, want %q", r.ID, "input-1")
	}
	if r.Status != "success" {
		t.Errorf("Status: got %q, want %q", r.Status, "success")
	}
}

// TestSidecar_OutputUpload tests that the sidecar uploads a post-job artifact
// and reports the result to the orchestrator endpoint.
func TestSidecar_OutputUpload(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	dockerClient, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("Failed to create Docker client: %v", err)
	}
	defer dockerClient.Close()

	sharedDir := t.TempDir()

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

	var reportCount atomic.Int64
	var mu sync.Mutex
	var receivedReports []job.ArtifactReport
	orchestratorServer := mockOrchestratorServer(t, &reportCount, &mu, &receivedReports)
	defer orchestratorServer.Close()

	reader, err := dockerClient.ImagePull(ctx, "alpine:latest", client.ImagePullOptions{})
	if err != nil {
		t.Fatalf("Failed to pull image: %v", err)
	}
	_, _ = io.Copy(io.Discard, reader)
	reader.Close()

	jobID := fmt.Sprintf("sidecar-output-%d", time.Now().UnixNano())
	containerName := fmt.Sprintf("job-%s-worker", jobID)

	outputContent := "this is the job output"
	resp, err := dockerClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: "alpine:latest",
			Cmd:   []string{"/bin/sh", "-c", fmt.Sprintf("echo -n '%s' > /workspace/result.txt", outputContent)},
		},
		HostConfig: &container.HostConfig{
			Binds: []string{sharedDir + ":/workspace"},
		},
		Name: containerName,
	})
	if err != nil {
		t.Fatalf("Failed to create container: %v", err)
	}
	defer func() {
		timeout := 5
		_, _ = dockerClient.ContainerStop(ctx, resp.ID, client.ContainerStopOptions{Timeout: &timeout})
		_, _ = dockerClient.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := dockerClient.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("Failed to start container: %v", err)
	}

	artifacts, err := artifact.UnmarshalArtifacts(fmt.Appendf(nil, `[{"id":"result","type":"upload","in":"result.txt","out":"%s","depends":"job"}]`, uploadServer.URL))
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	reporter := sidecar.NewHTTPSink(jobID, orchestratorServer.URL, "", 30*time.Second, "", "", nil, nil)

	runner := sidecar.NewRunner(jobID, sharedDir, 60,
		sidecar.WithArtifactListener(reporter),
	)

	sidecarDone := make(chan error, 1)
	go func() { sidecarDone <- runner.Run(ctx, artifacts) }()

	wait := dockerClient.ContainerWait(ctx, resp.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	statusCh, errCh := wait.Result, wait.Error
	select {
	case <-statusCh:
	case err := <-errCh:
		t.Fatalf("Error waiting for container: %v", err)
	}

	syscall.Kill(syscall.Getpid(), syscall.SIGUSR1)

	if err := <-sidecarDone; err != nil {
		t.Errorf("Sidecar run failed: %v", err)
	}

	testutil.MustWaitForCount(t, &uploadCount, 1, testutil.WithTimeout(10*time.Second))
	testutil.MustWaitForCount(t, &reportCount, 1, testutil.WithTimeout(10*time.Second))

	uploadMu.Lock()
	if string(uploadedContent) != outputContent {
		t.Errorf("Uploaded content mismatch: got %q, want %q", string(uploadedContent), outputContent)
	}
	uploadMu.Unlock()

	mu.Lock()
	defer mu.Unlock()

	if len(receivedReports) == 0 {
		t.Fatal("No artifact reports received")
	}
	r := receivedReports[0]
	if r.ID != "result" {
		t.Errorf("ArtifactID: got %q, want %q", r.ID, "result")
	}
	if r.Status != "success" {
		t.Errorf("Status: got %q, want %q", r.Status, "success")
	}
}

// Note: Job failure (exit code) monitoring is handled by the Docker orchestrator,
// not the sidecar. See internal/docker/docker_integration_test.go for exit code tests.
