package kubernetes

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"orchestrator/pkg/job"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// LifecycleWatcher watches a K8s job's worker container and emits backend-agnostic
// signals. Implementations adapt their native signals into job.Signals.
type LifecycleWatcher interface {
	// Watch blocks until the job completes or ctx is cancelled, calling emit
	// for each signal: optionally Started, then Exited or Failed.
	// LogLine signals may be interleaved after Started.
	Watch(ctx context.Context, namespace, jobID string, emit func(job.Signal))
}

// k8sLifecycleWatcher uses a single SharedInformer over managed pods in the
// configured namespace and dispatches events to per-job trackers. This avoids
// opening a separate Watch stream per running job.
type k8sLifecycleWatcher struct {
	client    kubernetes.Interface
	namespace string

	startOnce sync.Once
	factory   informers.SharedInformerFactory
	synced    chan struct{}

	mu       sync.Mutex
	trackers map[string]*jobTracker
}

func newK8sLifecycleWatcher(client kubernetes.Interface, namespace string) *k8sLifecycleWatcher {
	return &k8sLifecycleWatcher{
		client:    client,
		namespace: namespace,
		trackers:  make(map[string]*jobTracker),
		synced:    make(chan struct{}),
	}
}

// start spins up the SharedInformer on first use. Safe to call from many
// goroutines; only the first call does the work.
func (w *k8sLifecycleWatcher) start(ctx context.Context) {
	w.startOnce.Do(func() {
		labelSelector := LabelManagedBy + "=" + ManagedByValue
		factory := informers.NewSharedInformerFactoryWithOptions(
			w.client,
			30*time.Second,
			informers.WithNamespace(w.namespace),
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.LabelSelector = labelSelector
			}),
		)
		w.factory = factory

		informer := factory.Core().V1().Pods().Informer()
		_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if pod, ok := obj.(*corev1.Pod); ok {
					w.dispatch(ctx, pod, false)
				}
			},
			UpdateFunc: func(_, newObj any) {
				if pod, ok := newObj.(*corev1.Pod); ok {
					w.dispatch(ctx, pod, false)
				}
			},
			DeleteFunc: func(obj any) {
				if pod, ok := obj.(*corev1.Pod); ok {
					w.dispatch(ctx, pod, true)
				}
			},
		})

		stopCh := make(chan struct{})
		go func() {
			<-ctx.Done()
			close(stopCh)
		}()
		factory.Start(stopCh)
		factory.WaitForCacheSync(stopCh)
		close(w.synced)
	})
}

// dispatch routes a pod event to its tracker (if any).
func (w *k8sLifecycleWatcher) dispatch(ctx context.Context, pod *corev1.Pod, deleted bool) {
	jobID := pod.Labels[LabelJobID]
	if jobID == "" {
		return
	}
	w.mu.Lock()
	t, ok := w.trackers[jobID]
	w.mu.Unlock()
	if !ok {
		return
	}
	if deleted {
		t.handleDelete()
		return
	}
	t.handleUpdate(ctx, pod)
}

// Watch registers a tracker for jobID, replays any pod already in the informer
// cache, and blocks until the tracker observes terminal state or ctx cancels.
// The namespace argument is ignored; the watcher uses the namespace it was
// configured with (all managed jobs live in that single namespace).
func (w *k8sLifecycleWatcher) Watch(ctx context.Context, _, jobID string, emit func(job.Signal)) {
	w.start(ctx)
	select {
	case <-w.synced:
	case <-ctx.Done():
		return
	}

	t := newJobTracker(w, jobID, emit)

	w.mu.Lock()
	w.trackers[jobID] = t
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.trackers, jobID)
		w.mu.Unlock()
		t.stopLogs()
	}()

	// Replay any pod already in the informer cache so callers entering Watch
	// after a Pod has been observed still see the full transition history.
	podLister := w.factory.Core().V1().Pods().Lister().Pods(w.namespace)
	selector := labels.SelectorFromSet(map[string]string{LabelJobID: jobID})
	if pods, err := podLister.List(selector); err == nil {
		for _, pod := range pods {
			t.handleUpdate(ctx, pod)
		}
	}

	select {
	case <-ctx.Done():
	case <-t.done:
	}
}

// jobTracker owns the per-job state machine driven by pod events from the
// shared informer. Serialized via the mutex; handleUpdate/handleDelete may be
// called concurrently by informer goroutines.
type jobTracker struct {
	watcher *k8sLifecycleWatcher
	jobID   string
	emit    func(job.Signal)
	logger  *slog.Logger
	done    chan struct{}

	mu     sync.Mutex
	state  watcherState
	closed bool
}

// watcherState is the per-job mutable state. Guarded by jobTracker.mu.
type watcherState struct {
	isStarted bool
	isExited  bool
	startTime time.Time
	logCancel context.CancelFunc
	logDone   chan struct{}
}

func newJobTracker(w *k8sLifecycleWatcher, jobID string, emit func(job.Signal)) *jobTracker {
	return &jobTracker{
		watcher: w,
		jobID:   jobID,
		emit:    emit,
		logger:  slog.With("namespace", w.namespace, "jobId", jobID),
		done:    make(chan struct{}),
	}
}

func (t *jobTracker) handleUpdate(ctx context.Context, pod *corev1.Pod) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if done := t.applyPodStateLocked(ctx, pod); done {
		t.closeLocked()
	}
}

func (t *jobTracker) handleDelete() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if !t.state.isExited {
		t.emit(job.Failed{Reason: "pod deleted"})
	}
	t.closeLocked()
}

func (t *jobTracker) closeLocked() {
	if t.closed {
		return
	}
	t.closed = true
	close(t.done)
}

// applyPodStateLocked advances the state machine for one pod update.
// Returns true when the job reaches terminal state and the tracker should stop.
// Caller must hold t.mu.
func (t *jobTracker) applyPodStateLocked(ctx context.Context, pod *corev1.Pod) bool {
	var worker *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == ContainerWorker {
			worker = cs
			break
		}
	}

	// Pod-level failure before the worker ever ran (e.g. artifact-pre init
	// failure, ImagePullBackOff, scheduler rejection).
	if !t.state.isStarted && pod.Status.Phase == corev1.PodFailed && worker == nil {
		reason := pod.Status.Reason
		if reason == "" {
			reason = "pod failed before worker started"
		}
		t.emit(job.Failed{Reason: reason})
		return true
	}

	if worker == nil {
		return false
	}

	if !t.state.isStarted && worker.State.Running != nil {
		t.state.isStarted = true
		t.state.startTime = worker.State.Running.StartedAt.Time
		if t.state.startTime.IsZero() {
			t.state.startTime = time.Now()
		}
		t.logger.Info("Worker started")
		t.emit(job.Started{})
		t.startLogsLocked(ctx, pod.Name)
	}

	// Resume path: worker already terminated when we first observe it.
	if !t.state.isStarted && worker.State.Terminated != nil {
		t.state.isStarted = true
		t.state.startTime = worker.State.Terminated.StartedAt.Time
		t.emit(job.Started{})
	}

	if t.state.isStarted && !t.state.isExited && worker.State.Terminated != nil {
		t.state.isExited = true
		exitCode := int(worker.State.Terminated.ExitCode)
		duration := time.Duration(0)
		if !worker.State.Terminated.FinishedAt.IsZero() && !t.state.startTime.IsZero() {
			duration = worker.State.Terminated.FinishedAt.Sub(t.state.startTime)
		}
		t.logger.Info("Worker exited", "exitCode", exitCode)
		time.Sleep(500 * time.Millisecond) // allow log flush
		t.stopLogsLocked()
		t.emit(job.Exited{ExitCode: exitCode, Duration: duration})
		return true
	}

	return false
}

func (t *jobTracker) startLogsLocked(ctx context.Context, podName string) {
	if t.state.logCancel != nil {
		return
	}
	logCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.streamLogs(logCtx, podName)
	}()
	t.state.logCancel = cancel
	t.state.logDone = done
}

// stopLogs cancels log streaming if active. Public entry; acquires the mutex.
func (t *jobTracker) stopLogs() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopLogsLocked()
}

func (t *jobTracker) stopLogsLocked() {
	if t.state.logCancel == nil {
		return
	}
	t.state.logCancel()
	done := t.state.logDone
	t.state.logCancel = nil
	t.state.logDone = nil
	if done != nil {
		// Release the mutex while waiting so log-streaming goroutines that
		// still need to check t.state (none today, but defensive) don't deadlock.
		t.mu.Unlock()
		<-done
		t.mu.Lock()
	}
}

func (t *jobTracker) streamLogs(ctx context.Context, podName string) {
	req := t.watcher.client.CoreV1().Pods(t.watcher.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container:  ContainerWorker,
		Follow:     true,
		Timestamps: true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		t.logger.Warn("Failed to stream logs", "error", err)
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
		t.emit(job.LogLine{Stream: "stdout", Lines: batch})
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
		t.logger.Debug("Log stream ended", "error", err)
	}
}
