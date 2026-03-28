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
	"testing"
	"time"
)

const sidecarImage = "ko.local/job-sidecar:latest"

func TestOrchestrator_EventBasedFlow(t *testing.T) {
	ctx := context.Background()

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}

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
	if err := orchestrator.Run(ctx, req); err != nil {
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

func TestOrchestrator_DownloadFailure(t *testing.T) {
	ctx := context.Background()

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}

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
				In:  "http://127.0.0.1:1/nonexistent",
			},
		},
	}

	if err := orchestrator.Run(ctx, req); err != nil {
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

	d := dispatcher.NewMemoryDispatcher(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2}, nil)
	defer d.Close(ctx)

	emitter := job.NewEventEmitter()
	emitter.OnEvent(job.EventListenerFunc(func(e *job.Event) {
		if e.CallbackURL == "" {
			return
		}
		_ = d.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		})
	}))

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(emitter)
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}

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

	if err := orchestrator.Run(ctx, req); err != nil {
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

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}

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

	if err := orchestrator.Run(ctx, req); err != nil {
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

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}

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

	if err := orchestrator.Run(ctx, req); err != nil {
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

	orchestrator, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.Close()
	if err := orchestrator.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator: %v", err)
	}

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

	if err := orchestrator.Run(ctx, req); err != nil {
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
	if err := orchestrator.Stop(ctx, jobID); err != nil {
		t.Fatalf("Failed to stop job: %v", err)
	}

	// Verify job is gone
	_, err = orchestrator.Status(ctx, jobID)
	if err == nil {
		t.Error("Expected error getting status of stopped job")
	}
}

// --- Resume / Reconciliation Tests ---
//
// These tests simulate a service restart by closing one orchestrator instance
// (which cancels watchers but leaves containers running) and creating a new
// instance whose Start() reconciles the in-flight jobs.

func TestOrchestrator_ResumeRunningJob(t *testing.T) {
	ctx := context.Background()

	// Phase 1: Start a long-running job with orchestrator A
	orchestratorA, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator A: %v", err)
	}
	if err := orchestratorA.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator A: %v", err)
	}

	jobID := fmt.Sprintf("resume-running-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "echo 'resumable' && sleep 10",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}

	if err := orchestratorA.Run(ctx, req); err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for worker to be running
	testutil.MustWaitFor(t, func() bool {
		status, err := orchestratorA.Status(ctx, jobID)
		if err != nil {
			return false
		}
		return status.State == job.StateRunning
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Phase 2: Simulate service restart — close A, create B
	orchestratorA.Close()

	orchestratorB, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator B: %v", err)
	}
	defer orchestratorB.Close()
	if err := orchestratorB.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator B: %v", err)
	}

	// Phase 3: Verify job was resumed and is still running
	status, err := orchestratorB.Status(ctx, jobID)
	if err != nil {
		t.Fatalf("Failed to get status from B: %v", err)
	}
	if status.State != job.StateRunning {
		t.Errorf("Expected running state after resume, got %s", status.State)
	}

	// Wait for the job to complete naturally
	var finalStatus *job.Status
	testutil.MustWaitFor(t, func() bool {
		finalStatus, err = orchestratorB.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return finalStatus.State == job.StateCompleted || finalStatus.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	if finalStatus.State != job.StateCompleted {
		t.Errorf("Expected completed state, got %s", finalStatus.State)
	}
	if finalStatus.ExitCode == nil || *finalStatus.ExitCode != 0 {
		t.Errorf("Expected exit code 0, got %v", finalStatus.ExitCode)
	}

	_ = orchestratorB.Stop(ctx, jobID)
}

func TestOrchestrator_ResumeCallbackEvents(t *testing.T) {
	ctx := context.Background()

	// Callback server tracks events received after restart
	var receivedAfterRestart []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedAfterRestart = append(receivedAfterRestart, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Phase 1: Start job with orchestrator A (with callbacks)
	dA := dispatcher.NewMemoryDispatcher(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2, HTTPTimeout: 5 * time.Second}, nil)
	emitterA := job.NewEventEmitter()
	emitterA.OnEvent(job.EventListenerFunc(func(e *job.Event) {
		if e.CallbackURL == "" {
			return
		}
		_ = dA.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		})
	}))

	orchestratorA, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(emitterA)
	if err != nil {
		t.Fatalf("Failed to create orchestrator A: %v", err)
	}
	if err := orchestratorA.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator A: %v", err)
	}

	jobID := fmt.Sprintf("resume-callback-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "echo 'callback after resume' && sleep 10",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
		Callback: &job.Callback{
			URL:    server.URL,
			Events: []string{"orchestrator.job.start", "orchestrator.job.exit"},
		},
	}

	if err := orchestratorA.Run(ctx, req); err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	// Wait for worker to be running
	testutil.MustWaitFor(t, func() bool {
		status, err := orchestratorA.Status(ctx, jobID)
		if err != nil {
			return false
		}
		return status.State == job.StateRunning
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Phase 2: Simulate restart — close A's stack, reset event tracking
	orchestratorA.Close()
	dA.Close(ctx)

	mu.Lock()
	receivedAfterRestart = nil
	mu.Unlock()

	// Create orchestrator B with its own dispatcher + emitter
	dB := dispatcher.NewMemoryDispatcher(dispatcher.MemoryConfig{BufferSize: 100, Workers: 2, HTTPTimeout: 5 * time.Second}, nil)
	defer dB.Close(ctx)
	emitterB := job.NewEventEmitter()
	emitterB.OnEvent(job.EventListenerFunc(func(e *job.Event) {
		if e.CallbackURL == "" {
			return
		}
		_ = dB.Dispatch(&dispatcher.Event{
			Payload:     e.Payload,
			Destination: e.CallbackURL,
			SigningKey:  e.SigningKey,
		})
	}))

	orchestratorB, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(emitterB)
	if err != nil {
		t.Fatalf("Failed to create orchestrator B: %v", err)
	}
	defer orchestratorB.Close()
	if err := orchestratorB.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator B: %v", err)
	}

	// Phase 3: Wait for job to complete under orchestrator B
	testutil.MustWaitFor(t, func() bool {
		status, err := orchestratorB.Status(ctx, jobID)
		if err != nil {
			return true
		}
		return status.State == job.StateCompleted || status.State == job.StateFailed
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Wait for the exit callback to be delivered
	testutil.MustWaitFor(t, func() bool {
		mu.Lock()
		count := len(receivedAfterRestart)
		mu.Unlock()
		return count >= 1
	}, testutil.WithTimeout(10*time.Second), testutil.WithInterval(500*time.Millisecond))

	mu.Lock()
	eventCount := len(receivedAfterRestart)
	mu.Unlock()

	// Should have received at least the exit event from B
	if eventCount < 1 {
		t.Errorf("Expected at least 1 callback event after resume, got %d", eventCount)
	}

	_ = orchestratorB.Stop(ctx, jobID)
}

func TestOrchestrator_ResumeJobAppearsinList(t *testing.T) {
	ctx := context.Background()

	// Phase 1: Start a job with orchestrator A
	orchestratorA, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator A: %v", err)
	}
	if err := orchestratorA.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator A: %v", err)
	}

	jobID := fmt.Sprintf("resume-list-%d", time.Now().UnixNano())
	req := &job.Request{
		ID:             jobID,
		Image:          "alpine:latest",
		Command:        "sleep 30",
		CPU:            1,
		Memory:         128,
		TimeoutSeconds: 60,
		Workspace:      "/workspace",
	}

	if err := orchestratorA.Run(ctx, req); err != nil {
		t.Fatalf("Failed to run job: %v", err)
	}

	testutil.MustWaitFor(t, func() bool {
		status, err := orchestratorA.Status(ctx, jobID)
		if err != nil {
			return false
		}
		return status.State == job.StateRunning
	}, testutil.WithTimeout(60*time.Second), testutil.WithInterval(time.Second))

	// Phase 2: Restart
	orchestratorA.Close()

	orchestratorB, err := NewOrchestrator(ctx, Config{
		SidecarImage: sidecarImage,
	})(job.NewEventEmitter())
	if err != nil {
		t.Fatalf("Failed to create orchestrator B: %v", err)
	}
	defer orchestratorB.Close()
	if err := orchestratorB.Start(ctx); err != nil {
		t.Fatalf("Failed to start orchestrator B: %v", err)
	}

	// Phase 3: Job should appear in List
	jobs, err := orchestratorB.List(ctx)
	if err != nil {
		t.Fatalf("Failed to list jobs: %v", err)
	}

	found := false
	for _, j := range jobs {
		if j.ID == jobID {
			found = true
			if j.State != job.StateRunning {
				t.Errorf("Expected running state in list, got %s", j.State)
			}
			break
		}
	}
	if !found {
		t.Errorf("Resumed job %s not found in list", jobID)
	}

	_ = orchestratorB.Stop(ctx, jobID)
}
