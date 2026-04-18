package kubernetes

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"orchestrator/internal/job"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// LifecycleWatcher watches a K8s job's worker container and emits backend-agnostic
// signals. Implementations adapt their native signals into job.Signals.
type LifecycleWatcher interface {
	// Watch blocks until the job completes or ctx is cancelled, calling emit
	// for each signal: optionally Started, then Exited or Failed.
	// LogLine signals may be interleaved after Started.
	Watch(ctx context.Context, namespace, jobID string, emit func(job.Signal))
}

type k8sLifecycleWatcher struct {
	client kubernetes.Interface
}

func newK8sLifecycleWatcher(c kubernetes.Interface) *k8sLifecycleWatcher {
	return &k8sLifecycleWatcher{client: c}
}

// watcherState tracks mutable state across reconnect iterations.
type watcherState struct {
	isStarted bool
	isExited  bool
	startTime time.Time
	logCancel context.CancelFunc
	logDone   chan struct{}
}

func (w *k8sLifecycleWatcher) Watch(ctx context.Context, namespace, jobID string, emit func(job.Signal)) {
	logger := slog.With("namespace", namespace, "jobId", jobID)
	state := &watcherState{}
	defer w.stopLogs(state)

	for {
		if ctx.Err() != nil {
			return
		}

		selector := LabelJobID + "=" + jobID
		podWatch, err := w.client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			logger.Warn("Failed to watch pods", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}

		if done := w.process(ctx, logger, namespace, state, emit, podWatch); done {
			return
		}

		logger.Warn("Pod watch disconnected, reconnecting...")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (w *k8sLifecycleWatcher) process(ctx context.Context, logger *slog.Logger, namespace string, state *watcherState, emit func(job.Signal), podWatch watch.Interface) bool {
	defer podWatch.Stop()

	for {
		select {
		case <-ctx.Done():
			return true
		case ev, ok := <-podWatch.ResultChan():
			if !ok {
				return false
			}
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			if ev.Type == watch.Deleted {
				if !state.isExited {
					emit(job.Failed{Reason: "pod deleted"})
				}
				return true
			}
			if done := w.applyPodState(ctx, logger, namespace, pod, state, emit); done {
				return true
			}
		}
	}
}

// applyPodState inspects the worker container status and emits signals for transitions.
// Returns true when the job is terminal and watching should stop.
func (w *k8sLifecycleWatcher) applyPodState(ctx context.Context, logger *slog.Logger, namespace string, pod *corev1.Pod, state *watcherState, emit func(job.Signal)) bool {
	var worker *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == ContainerWorker {
			worker = cs
			break
		}
	}

	// Pod-level failure before the worker ever started (e.g. artifact-pre init failure,
	// ImagePullBackOff, scheduler rejection).
	if !state.isStarted && pod.Status.Phase == corev1.PodFailed && worker == nil {
		reason := pod.Status.Reason
		if reason == "" {
			reason = "pod failed before worker started"
		}
		emit(job.Failed{Reason: reason})
		return true
	}

	if worker == nil {
		return false
	}

	if !state.isStarted && worker.State.Running != nil {
		state.isStarted = true
		state.startTime = worker.State.Running.StartedAt.Time
		if state.startTime.IsZero() {
			state.startTime = time.Now()
		}
		logger.Info("Worker started")
		emit(job.Started{})
		state.logCancel, state.logDone = w.startLogs(ctx, logger, namespace, pod.Name, emit)
	}

	// Resume path: worker already terminated when we first observe it.
	if !state.isStarted && worker.State.Terminated != nil {
		state.isStarted = true
		state.startTime = worker.State.Terminated.StartedAt.Time
		emit(job.Started{})
	}

	if state.isStarted && !state.isExited && worker.State.Terminated != nil {
		state.isExited = true
		exitCode := int(worker.State.Terminated.ExitCode)
		duration := time.Duration(0)
		if !worker.State.Terminated.FinishedAt.IsZero() && !state.startTime.IsZero() {
			duration = worker.State.Terminated.FinishedAt.Sub(state.startTime)
		}
		logger.Info("Worker exited", "exitCode", exitCode)
		time.Sleep(500 * time.Millisecond) // allow log flush
		w.stopLogs(state)
		emit(job.Exited{ExitCode: exitCode, Duration: duration})
		return true
	}

	return false
}

func (w *k8sLifecycleWatcher) startLogs(ctx context.Context, logger *slog.Logger, namespace, podName string, emit func(job.Signal)) (context.CancelFunc, chan struct{}) {
	logCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.streamLogs(logCtx, logger, namespace, podName, emit)
	}()
	return cancel, done
}

func (w *k8sLifecycleWatcher) stopLogs(state *watcherState) {
	if state.logCancel != nil {
		state.logCancel()
		<-state.logDone
		state.logCancel = nil
		state.logDone = nil
	}
}

func (w *k8sLifecycleWatcher) streamLogs(ctx context.Context, logger *slog.Logger, namespace, podName string, emit func(job.Signal)) {
	req := w.client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container:  ContainerWorker,
		Follow:     true,
		Timestamps: true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		logger.Warn("Failed to stream logs", "error", err)
		return
	}
	defer stream.Close()

	// The K8s logs API returns a single stream; stdout/stderr are interleaved.
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var batch []string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		emit(job.LogLine{Stream: "stdout", Lines: batch})
		batch = nil
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			continue
		}
		batch = append(batch, line)
		if len(batch) >= 32 {
			flush()
		}
	}
	flush()
	if err := scanner.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		logger.Debug("Log stream ended", "error", err)
	}
}
