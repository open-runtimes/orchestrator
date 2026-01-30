//go:build integration

package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/job"
	"orchestrator/internal/sidecar"
	"orchestrator/internal/testutil"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const sidecarImage = "ko.local/job-sidecar:latest"

func TestOrchestrator_EventBasedFlow(t *testing.T) {
	ctx := context.Background()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("event-flow-test-%d", time.Now().UnixNano())

	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "echo 'hello from event flow test' && sleep 1",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}

	// Run job
	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Initially should be accepted (waiting for sidecar)
	status, err := orchestrator.Status(ctx, jobID)
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status.State != job.StateAccepted && status.State != job.StateRunning {
		t.Errorf("Expected accepted or running state, got %s", status.State)
	}

	// Wait for completion
	var finalStatus *job.Status
	testutil.MustWaitFor(t, func() bool {
		finalStatus, err = orchestrator.Status(ctx, jobID)
		if err != nil {
			return true // Job may have been cleaned up
		}
		return finalStatus.State == job.StateCompleted || finalStatus.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	if finalStatus.State != job.StateCompleted {
		t.Errorf("Expected completed state, got %s", finalStatus.State)
	}
	if finalStatus.ExitCode == nil || *finalStatus.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %v", finalStatus.ExitCode)
	}

	// Cleanup
	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_WithDownload(t *testing.T) {
	ctx := context.Background()

	// Create a test server to serve the input file
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("test input content"))
	}))
	defer server.Close()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("download-test-%d", time.Now().UnixNano())

	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "cat /workspace/input.txt && echo ' - processed'",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Artifacts: []artifact.Artifact{
			&artifact.Download{
				ID:  "test-input",
				Out: "input.txt",
				In:  server.URL,
			},
		},
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for completion
	var finalStatus *job.Status
	testutil.MustWaitFor(t, func() bool {
		finalStatus, err = orchestrator.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return finalStatus.State == job.StateCompleted || finalStatus.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	if finalStatus.State != job.StateCompleted {
		t.Errorf("Expected completed state, got %s", finalStatus.State)
	}

	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_WithUpload(t *testing.T) {
	ctx := context.Background()

	// Create a test server to receive uploads
	var uploadReceived atomic.Bool
	var uploadContent []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			uploadContent, _ = io.ReadAll(r.Body)
			uploadReceived.Store(true)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("upload-test-%d", time.Now().UnixNano())

	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "echo 'output content' > /workspace/output.txt",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Artifacts: []artifact.Artifact{
			&artifact.Upload{
				ID:      "test-output",
				In:      "output.txt",
				Out:     server.URL,
				Depends: artifact.JobDependency,
			},
		},
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for completion
	testutil.MustWaitFor(t, func() bool {
		status, err := orchestrator.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return status.State == job.StateCompleted || status.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Verify upload was received
	if !uploadReceived.Load() {
		t.Error("Upload was not received")
	}
	if len(uploadContent) == 0 {
		t.Error("Upload content was empty")
	}

	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_DownloadFailure(t *testing.T) {
	ctx := context.Background()

	// Create a server that returns 404
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("download-fail-test-%d", time.Now().UnixNano())

	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "echo 'should not run'",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 30,
		Workspace:      "/workspace",
		Artifacts: []artifact.Artifact{
			&artifact.Download{
				ID:  "bad-input",
				Out: "input.txt",
				In:  server.URL + "/nonexistent",
			},
		},
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for job to fail (sidecar should exit with error)
	var finalStatus *job.Status
	testutil.MustWaitFor(t, func() bool {
		finalStatus, err = orchestrator.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return finalStatus.State == job.StateCompleted || finalStatus.State == job.StateFailed
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(time.Second))

	// Job should fail because download failed
	if finalStatus.State != job.StateFailed {
		t.Errorf("Expected failed state, got %s", finalStatus.State)
	}

	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_CallbackEvents(t *testing.T) {
	ctx := context.Background()

	// Track received events
	var receivedEvents []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedEvents = append(receivedEvents, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage:     sidecarImage,
		Dispatcher:       d,
		CallbackProxyURL: "", // Direct callbacks for test
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("callback-test-%d", time.Now().UnixNano())

	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "echo 'hello'",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Callback: &job.Callback{
			URL:    server.URL,
			Events: []string{"orchestrator.job.start", "orchestrator.job.exit"},
		},
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for completion
	testutil.MustWaitFor(t, func() bool {
		status, err := orchestrator.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return status.State == job.StateCompleted || status.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Wait a bit for events to be delivered
	time.Sleep(2 * time.Second)

	mu.Lock()
	eventCount := len(receivedEvents)
	mu.Unlock()

	// Should receive start and exit events
	if eventCount < 2 {
		t.Errorf("Expected at least 2 events (start, exit), got %d", eventCount)
	}

	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_HealthCheckMarker(t *testing.T) {
	// Verify the sidecar writes the ready marker file
	ctx := context.Background()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("marker-test-%d", time.Now().UnixNano())

	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        fmt.Sprintf("test -f /workspace/%s && echo 'marker exists'", sidecar.ReadyFile),
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for completion
	var finalStatus *job.Status
	testutil.MustWaitFor(t, func() bool {
		finalStatus, err = orchestrator.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return finalStatus.State == job.StateCompleted || finalStatus.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Job should complete successfully (marker file should exist)
	if finalStatus.State != job.StateCompleted {
		t.Errorf("Expected completed state (marker file should exist), got %s", finalStatus.State)
	}

	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_List(t *testing.T) {
	ctx := context.Background()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	// Create a job
	jobID := fmt.Sprintf("list-test-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "sleep 30",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for job to be listed
	testutil.MustWaitFor(t, func() bool {
		jobs, err := orchestrator.List(ctx)
		if err != nil {
			return false
		}
		for _, j := range jobs {
			if j.ID == jobID {
				return true
			}
		}
		return false
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(time.Second))

	// Cleanup
	_ = orchestrator.Stop(ctx, jobID)
}

func TestOrchestrator_Stop(t *testing.T) {
	ctx := context.Background()

	d := dispatcher.NewMemory(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
		Dispatcher:   d,
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()

	jobID := fmt.Sprintf("stop-test-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "sleep 300",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}

	err = orchestrator.Run(ctx, req)
	if err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for job to start running
	testutil.MustWaitFor(t, func() bool {
		status, err := orchestrator.Status(ctx, jobID)
		if err != nil {
			return false
		}
		return status.State == job.StateRunning
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Stop job
	err = orchestrator.Stop(ctx, jobID)
	if err != nil {
		t.Fatalf("Failed to stop job: %v", err)
	}

	// Verify job is gone
	_, err = orchestrator.Status(ctx, jobID)
	if err == nil {
		t.Error("Expected error getting status of stopped job")
	}
}
