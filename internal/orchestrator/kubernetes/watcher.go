package kubernetes

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"orchestrator/internal/observability"
	"orchestrator/pkg/job"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// LifecycleWatcher observes managed-job Pods cluster-wide and emits callbacks
// for Started / Exited / Failed / LogLine signals. It is self-contained: the
// orchestrator's only interaction is Start(ctx) — tracker lifecycle, callback
// emission, and log streaming are all handled internally.
//
// Start blocks until ctx cancels, at which point all in-flight trackers are
// torn down and the informer stops. A single watcher instance is reusable via
// repeated Start calls with fresh contexts (each call is a fresh leadership
// term in the leader-elected deployment).
type LifecycleWatcher interface {
	Start(ctx context.Context)
}

// k8sLifecycleWatcher runs a SharedInformer over Pods labelled as managed-by
// jobs-service and materialises a jobTracker for each unique job.id. Trackers
// drive the per-job state machine and emit callbacks directly via the shared
// emitter.
type k8sLifecycleWatcher struct {
	client    kubernetes.Interface
	namespace string
	emitter   *job.CallbackEmitter
	metrics   *observability.Metrics // may be nil in tests

	mu       sync.Mutex
	trackers map[string]*jobTracker
}

func newK8sLifecycleWatcher(client kubernetes.Interface, namespace string, emitter *job.CallbackEmitter, metrics *observability.Metrics) *k8sLifecycleWatcher {
	return &k8sLifecycleWatcher{
		client:    client,
		namespace: namespace,
		emitter:   emitter,
		metrics:   metrics,
		trackers:  make(map[string]*jobTracker),
	}
}

// Start runs the informer until ctx cancels, then tears down all trackers.
// Safe to call repeatedly (e.g. on each leader-acquire) because a new
// informer factory is built per call.
func (w *k8sLifecycleWatcher) Start(ctx context.Context) {
	labelSelector := LabelManagedBy + "=" + ManagedByValue
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.client,
		30*time.Second,
		informers.WithNamespace(w.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labelSelector
		}),
	)

	informer := factory.Core().V1().Pods().Informer()
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.handle(ctx, pod, false)
			}
		},
		UpdateFunc: func(_, newObj any) {
			if pod, ok := newObj.(*corev1.Pod); ok {
				w.handle(ctx, pod, false)
			}
		},
		DeleteFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok {
				w.handle(ctx, pod, true)
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

	<-stopCh

	// Leadership term ended (or Close called). Cancel all in-flight trackers
	// so their log-streaming goroutines exit and no more callbacks fire.
	w.mu.Lock()
	dropped := int64(len(w.trackers))
	for _, t := range w.trackers {
		t.close()
	}
	w.trackers = make(map[string]*jobTracker)
	w.mu.Unlock()
	if dropped > 0 && w.metrics != nil {
		w.metrics.RecordTrackerDelta(context.Background(), -dropped)
	}
}

// handle routes a pod event to its tracker, creating one on first observation.
// Trackers stay in the map after reaching terminal state (marked closed) so
// that post-terminal Pod updates — which K8s emits routinely (status tweaks,
// resyncs) — don't cause a new tracker to be created and the state machine to
// replay. The tracker is only evicted when the Pod itself is deleted.
func (w *k8sLifecycleWatcher) handle(ctx context.Context, pod *corev1.Pod, deleted bool) {
	jobID := pod.Labels[LabelJobID]
	if jobID == "" {
		return
	}

	w.mu.Lock()
	t, ok := w.trackers[jobID]
	created := false
	if !ok {
		if deleted {
			// Pod deleted and we never saw it — nothing to do.
			w.mu.Unlock()
			return
		}
		t = newJobTracker(w, watchConfigFromPod(pod))
		w.trackers[jobID] = t
		created = true
	}
	w.mu.Unlock()

	if created && w.metrics != nil {
		w.metrics.RecordTrackerDelta(ctx, 1)
	}

	if deleted {
		t.handleDelete()
		w.mu.Lock()
		removed := false
		if cur, ok := w.trackers[jobID]; ok && cur == t {
			delete(w.trackers, jobID)
			removed = true
		}
		w.mu.Unlock()
		if removed && w.metrics != nil {
			w.metrics.RecordTrackerDelta(ctx, -1)
		}
		return
	}
	t.handleUpdate(ctx, pod)
}

// watchConfigFromPod derives the callback destination from a Pod's annotations
// and labels. Mirrors watchConfigFromJob; used when the watcher first observes
// a Pod and needs to know where to dispatch callbacks for its job.
func watchConfigFromPod(pod *corev1.Pod) *watchConfig {
	cfg := &watchConfig{jobID: pod.Labels[LabelJobID]}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name == ContainerWorker {
			cfg.image = c.Image
			break
		}
	}
	cfg.dest = callbackDestFromAnnotations(pod.Annotations)
	return cfg
}

// jobTracker owns the per-job state machine. Events arriving via
// handleUpdate/handleDelete drive FSM transitions and callback emission.
type jobTracker struct {
	watcher *k8sLifecycleWatcher
	cfg     *watchConfig
	logger  *slog.Logger

	mu         sync.Mutex
	state      trackerState
	closed     bool
	closedOnce sync.Once
}

// trackerState is the per-job mutable state. Guarded by jobTracker.mu.
type trackerState struct {
	isStarted   bool
	isExited    bool
	startTime   time.Time
	logCancel   context.CancelFunc
	logDone     chan struct{}
	logSequence uint64
}

func newJobTracker(w *k8sLifecycleWatcher, cfg *watchConfig) *jobTracker {
	return &jobTracker{
		watcher: w,
		cfg:     cfg,
		logger:  slog.With("namespace", w.namespace, "jobId", cfg.jobID),
	}
}

// handleUpdate advances the state machine for a pod update. Once terminal
// state is reached, the tracker closes itself — subsequent calls are no-ops.
func (t *jobTracker) handleUpdate(ctx context.Context, pod *corev1.Pod) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.applyPodStateLocked(ctx, pod) {
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

// close tears the tracker down; used when the watcher's context is cancelled
// (leadership lost / orchestrator closed).
func (t *jobTracker) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeLocked()
}

func (t *jobTracker) closeLocked() {
	t.closedOnce.Do(func() {
		t.closed = true
		t.stopLogsLocked()
	})
}

func (t *jobTracker) emit(s job.Signal) {
	job.EmitCallback(t.watcher.emitter, t.cfg.jobID, t.cfg.image, t.cfg.dest, s)
}

func (t *jobTracker) nextLogSequence() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.logSequence++
	return t.state.logSequence
}

func (t *jobTracker) finalLogSequenceLocked() uint64 {
	return t.state.logSequence
}

// applyPodStateLocked advances the state machine for one pod update.
// Returns true when the job reaches terminal state. Caller must hold t.mu.
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

	// If we observe a Pod already in terminal state on first sight, assume a
	// previous leader (or the previous incarnation of this process before a
	// restart) already emitted Started + Exited callbacks. Mark the tracker
	// finished so subsequent events on this Pod are ignored — duplicates on
	// leader failover would otherwise double-fire the callback pipeline.
	if !t.state.isStarted && worker.State.Terminated != nil {
		t.state.isStarted = true
		t.state.isExited = true
		return true
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
		t.emit(job.Exited{ExitCode: exitCode, Duration: duration, FinalLogSequence: t.finalLogSequenceLocked()})
		return true
	}

	// Pod failed after the worker started (node loss, preemption, OOM-at-pod-
	// level). kubelet may mark the Pod Failed before the container-status
	// update lands, so we key off pod.Status.Phase here rather than waiting
	// for worker.State.Terminated.
	if t.state.isStarted && !t.state.isExited && pod.Status.Phase == corev1.PodFailed {
		reason := pod.Status.Reason
		if reason == "" {
			reason = "pod failed"
		}
		t.state.isExited = true
		t.logger.Info("Pod failed during job", "reason", reason)
		t.stopLogsLocked()
		t.emit(job.Failed{Reason: reason})
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

func (t *jobTracker) stopLogsLocked() {
	if t.state.logCancel == nil {
		return
	}
	t.state.logCancel()
	done := t.state.logDone
	t.state.logCancel = nil
	t.state.logDone = nil
	if done != nil {
		t.mu.Unlock()
		<-done
		t.mu.Lock()
	}
}

func (t *jobTracker) streamLogs(ctx context.Context, podName string) {
	req := t.watcher.client.CoreV1().Pods(t.watcher.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: ContainerWorker,
		Follow:    true,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		t.logger.Warn("Failed to stream logs", "error", err)
		return
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var batch []string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		t.emitLogBatch("stdout", batch)
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

func (t *jobTracker) emitLogBatch(stream string, lines []string) {
	if len(lines) == 0 {
		return
	}
	t.emit(job.LogLine{Stream: stream, Lines: lines, Sequence: t.nextLogSequence()})
}
