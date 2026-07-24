package docker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"orchestrator/pkg/job"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// LifecycleWatcher watches a sidecar+worker container pair and emits backend-agnostic
// signals. Backends implement this interface to adapt their native signals
// (Docker events, Kubernetes pod phases, etc.) into job.Signals.
type LifecycleWatcher interface {
	// Watch blocks until the job completes or ctx is cancelled, calling emit for
	// each signal in order: optionally Started, then Exited or Failed, then
	// Completed once the sidecar has finished post-job artifacts.
	// LogLine signals may be interleaved after Started.
	Watch(ctx context.Context, sidecarID, workerID string, emit func(job.Signal))
}

// dockerLifecycleWatcher implements LifecycleWatcher using the Docker API.
type dockerLifecycleWatcher struct {
	client *client.Client
}

func newDockerLifecycleWatcher(cli *client.Client) *dockerLifecycleWatcher {
	return &dockerLifecycleWatcher{client: cli}
}

// Watch blocks until the job completes or ctx is cancelled.
func (w *dockerLifecycleWatcher) Watch(ctx context.Context, sidecarID, workerID string, emit func(job.Signal)) {
	w.run(ctx, sidecarID, workerID, emit)
}

// watcherState tracks mutable state across reconnect iterations.
type watcherState struct {
	isWorkerStarted bool
	isWorkerExited  bool
	isWorkerOOM     bool
	startTime       time.Time
	logCancel       context.CancelFunc
	logDone         chan struct{}
}

func (w *dockerLifecycleWatcher) run(ctx context.Context, sidecarID, workerID string, out func(job.Signal)) {
	logger := slog.With("sidecarID", sidecarID)
	state := &watcherState{}

	defer func() {
		w.stopLogStreaming(state)
	}()

	for {
		if ctx.Err() != nil {
			return
		}

		// Subscribe to events BEFORE inspecting state to prevent a race where a
		// health_status event fires between the state check and the subscription.
		eventFilter := filters.NewArgs(
			filters.Arg("type", string(events.ContainerEventType)),
			filters.Arg("container", sidecarID),
			filters.Arg("container", workerID),
		)
		eventCh, errCh := w.client.Events(ctx, events.ListOptions{Filters: eventFilter})

		if done := w.reconcile(ctx, logger, sidecarID, workerID, state, out); done {
			return
		}

		if done := w.process(ctx, logger, sidecarID, workerID, state, out, eventCh, errCh); done {
			return
		}

		logger.Warn("Event stream disconnected, reconnecting...")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// reconcile inspects current container state and syncs watcherState accordingly.
// Returns true if the job is complete and watching should stop.
func (w *dockerLifecycleWatcher) reconcile(ctx context.Context, logger *slog.Logger, sidecarID, workerID string, state *watcherState, out func(job.Signal)) bool {
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 10*time.Second)
	defer inspectCancel()

	sidecar, err := w.client.ContainerInspect(inspectCtx, sidecarID)
	if err != nil {
		logger.Error("Failed to inspect sidecar during reconcile", "error", err)
		return true
	}

	if !sidecar.State.Running {
		switch {
		case !state.isWorkerStarted:
			logger.Error("Sidecar exited before inputs completed")
			out(job.Failed{Reason: "sidecar exited before inputs completed"})
		case state.isWorkerExited:
			// Sidecar exit missed during a stream disconnect; the worker exit
			// was already emitted, so only the completion is outstanding.
			out(job.Completed{})
		}
		return true
	}

	// Inspect the worker once for both resume detection and exit checking.
	type workerSnap struct {
		status    string
		running   bool
		exitCode  int
		oomKilled bool
		startedAt time.Time
		ok        bool
	}
	var ws workerSnap
	if info, err := w.client.ContainerInspect(inspectCtx, workerID); err == nil {
		ws.ok = true
		ws.status = info.State.Status
		ws.running = info.State.Running
		ws.exitCode = info.State.ExitCode
		ws.oomKilled = info.State.OOMKilled
		if t, parseErr := time.Parse(time.RFC3339Nano, info.State.StartedAt); parseErr == nil {
			ws.startedAt = t
		}
	}

	// Detect resume: worker was started in a previous session.
	if !state.isWorkerStarted && ws.ok {
		switch ws.status {
		case "running", "exited", "dead":
			state.isWorkerStarted = true
			state.startTime = ws.startedAt
		}
	}

	// Start worker if sidecar is healthy and worker not yet started.
	if !state.isWorkerStarted && sidecar.State.Health != nil && sidecar.State.Health.Status == "healthy" {
		logger.Info("Sidecar healthy (reconciled), starting worker")
		state.startTime = time.Now()
		if err := w.client.ContainerStart(ctx, workerID, container.StartOptions{}); err != nil {
			logger.Error("Failed to start worker", "error", err)
			out(job.Failed{Reason: "failed to start worker"})
			return true
		}
		state.isWorkerStarted = true
		ws.running = true
		out(job.Started{})
	}

	// Start or resume log streaming.
	if state.isWorkerStarted && state.logCancel == nil && !state.isWorkerExited {
		state.logCancel, state.logDone = w.startLogStreaming(ctx, logger, workerID, out)
	}

	// Handle worker exit detected via inspection (missed event on reconnect).
	if ws.ok && state.isWorkerStarted && !state.isWorkerExited && !ws.running {
		state.isWorkerExited = true
		duration := time.Duration(0)
		if !state.startTime.IsZero() {
			duration = time.Since(state.startTime)
		}
		reason := exitReason(ws.exitCode, state.isWorkerOOM || ws.oomKilled)
		logger.Info("Worker exited (reconciled)", "exitCode", ws.exitCode, "reason", reason)
		w.stopLogStreaming(state)
		if err := w.client.ContainerKill(ctx, sidecarID, "SIGUSR1"); err != nil {
			logger.Warn("Failed to signal sidecar", "error", err)
		}
		out(job.Exited{ExitCode: ws.exitCode, Reason: reason, Duration: duration})
	}

	return false
}

// process drains the event stream until job completion or a stream error.
// Returns true if the job is complete, false if reconnection is needed.
func (w *dockerLifecycleWatcher) process(ctx context.Context, logger *slog.Logger, sidecarID, workerID string, state *watcherState, out func(job.Signal), eventCh <-chan events.Message, errCh <-chan error) bool {
	for {
		select {
		case <-ctx.Done():
			return true

		case err := <-errCh:
			if err != nil {
				logger.Warn("Event stream error", "error", err)
			}
			return false // reconnect

		case event, ok := <-eventCh:
			if !ok {
				return false // channel closed; reconnect
			}

			switch {
			case event.Actor.ID == sidecarID && event.Action == "health_status: healthy" && !state.isWorkerStarted:
				logger.Info("Sidecar healthy, starting worker")
				state.startTime = time.Now()
				if err := w.client.ContainerStart(ctx, workerID, container.StartOptions{}); err != nil {
					logger.Error("Failed to start worker", "error", err)
					out(job.Failed{Reason: "failed to start worker"})
					return true
				}
				state.isWorkerStarted = true
				out(job.Started{})
				state.logCancel, state.logDone = w.startLogStreaming(ctx, logger, workerID, out)

			// The oom event fires even when the OOM killer takes a child
			// process rather than pid 1 — a case where the daemon may leave
			// State.OOMKilled unset (cgroup v1) — so it is tracked live and
			// combined with the inspect flag at exit.
			case event.Actor.ID == workerID && event.Action == "oom":
				state.isWorkerOOM = true

			case event.Actor.ID == workerID && event.Action == "die" && !state.isWorkerExited:
				state.isWorkerExited = true
				exitCode := w.parseExitCode(event)
				reason := exitReason(exitCode, state.isWorkerOOM || w.inspectOOMKilled(ctx, workerID))
				duration := time.Duration(0)
				if !state.startTime.IsZero() {
					duration = time.Since(state.startTime)
				}
				logger.Info("Worker exited", "exitCode", exitCode, "reason", reason)
				// Give logs a moment to flush before stopping the stream.
				time.Sleep(500 * time.Millisecond)
				w.stopLogStreaming(state)
				if err := w.client.ContainerKill(ctx, sidecarID, "SIGUSR1"); err != nil {
					logger.Warn("Failed to signal sidecar", "error", err)
				}
				out(job.Exited{ExitCode: exitCode, Reason: reason, Duration: duration})

			case event.Actor.ID == sidecarID && event.Action == "die":
				switch {
				case !state.isWorkerStarted:
					logger.Error("Sidecar exited before inputs completed")
					out(job.Failed{Reason: "sidecar exited before inputs completed"})
				case !state.isWorkerExited:
					logger.Warn("Sidecar exited while worker still running")
				default:
					logger.Info("Sidecar exited, job complete")
					out(job.Completed{})
				}
				return true
			}
		}
	}
}

func (w *dockerLifecycleWatcher) startLogStreaming(ctx context.Context, logger *slog.Logger, workerID string, out func(job.Signal)) (cancel context.CancelFunc, done chan struct{}) {
	logCtx, logCancel := context.WithCancel(ctx)
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		w.streamLogs(logCtx, logger, workerID, out)
	}()
	return logCancel, logDone
}

func (w *dockerLifecycleWatcher) stopLogStreaming(state *watcherState) {
	if state.logCancel != nil {
		state.logCancel()
		<-state.logDone
		state.logCancel = nil
		state.logDone = nil
	}
}

func (w *dockerLifecycleWatcher) streamLogs(ctx context.Context, logger *slog.Logger, containerID string, out func(job.Signal)) {
	logs, err := w.client.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		logger.Error("Failed to get container logs", "error", err)
		return
	}
	defer logs.Close()

	header := make([]byte, 8)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(logs, header); err != nil {
			if err != io.EOF && ctx.Err() == nil {
				logger.Debug("Log stream ended", "error", err)
			}
			return
		}

		size := int(header[4])<<24 | int(header[5])<<16 | int(header[6])<<8 | int(header[7])
		if size == 0 {
			continue
		}

		payload := make([]byte, size)
		if _, err := io.ReadFull(logs, payload); err != nil {
			logger.Debug("Failed to read log payload", "error", err)
			return
		}

		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}

		if lines := splitLines(string(payload)); len(lines) > 0 {
			out(job.LogLine{Stream: stream, Lines: lines})
		}
	}
}

// inspectOOMKilled reads State.OOMKilled off the exited worker, catching OOM
// kills whose oom event was missed during a stream reconnect.
func (w *dockerLifecycleWatcher) inspectOOMKilled(ctx context.Context, workerID string) bool {
	inspectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	info, err := w.client.ContainerInspect(inspectCtx, workerID)
	return err == nil && info.State.OOMKilled
}

// exitReason maps what the backend observed to an Exited.Reason. An OOM kill
// only counts when the worker actually died non-zero — the OOM killer may
// take a child process the worker's entrypoint survives.
func exitReason(exitCode int, oomKilled bool) string {
	if exitCode != 0 && oomKilled {
		return job.ExitReasonOOM
	}
	return ""
}

func (w *dockerLifecycleWatcher) parseExitCode(event events.Message) int {
	if code, ok := event.Actor.Attributes["exitCode"]; ok {
		var exitCode int
		if _, err := fmt.Sscanf(code, "%d", &exitCode); err == nil {
			return exitCode
		}
	}
	return -1
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")
	if strings.HasSuffix(s, "\n") {
		parts = parts[:len(parts)-1]
	}

	// Preserve blank lines from worker output so callback consumers receive
	// the same log spacing as the container produced.
	for i := range parts {
		parts[i] = strings.TrimSuffix(parts[i], "\r")
	}
	return parts
}
