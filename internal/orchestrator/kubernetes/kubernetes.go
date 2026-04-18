// Package kubernetes implements the job.Orchestrator interface using the Kubernetes API.
// Jobs run as batch/v1.Job resources in a configured namespace.
package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/job"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Orchestrator implements job.Orchestrator using Kubernetes.
type Orchestrator struct {
	client              kubernetes.Interface
	namespace           string
	sidecarImage        string
	cfg                 OrchestratorConfig
	retentionPeriod     time.Duration
	maintenanceInterval time.Duration
	emitter             *job.CallbackEmitter
	ctrl                *job.MemoryStore[kubernetesHandle]
	watcher             LifecycleWatcher
	statusCache         *statusCache

	cancelMaintenance context.CancelFunc
	watchWg           sync.WaitGroup
}

// Config holds configuration for the Kubernetes orchestrator.
type Config struct {
	SidecarImage                  string
	Kubeconfig                    string
	Namespace                     string
	ServiceAccount                string
	ImagePullSecrets              []string
	WorkerImagePullPolicy         string
	SidecarImagePullPolicy        string
	RetentionPeriod               time.Duration
	MaintenanceInterval           time.Duration
	ArtifactEndpoint              string
	TerminationGracePeriodSeconds int64
}

// NewOrchestrator returns an OrchestratorFactory that creates a Kubernetes orchestrator.
// Register listeners on the emitter before calling Start.
func NewOrchestrator(ctx context.Context, cfg Config) job.OrchestratorFactory {
	return func(emitter *job.CallbackEmitter) (job.Orchestrator, error) {
		restCfg, err := buildRestConfig(cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build kube config: %w", err)
		}
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create kube client: %w", err)
		}

		retention := cfg.RetentionPeriod
		if retention <= 0 {
			retention = 15 * time.Minute
		}
		maint := cfg.MaintenanceInterval
		if maint <= 0 {
			maint = 1 * time.Minute
		}
		ns := cfg.Namespace
		if ns == "" {
			ns = "orchestrator"
		}
		sa := cfg.ServiceAccount
		if sa == "" {
			sa = "job-sidecar"
		}
		grace := cfg.TerminationGracePeriodSeconds
		if grace <= 0 {
			grace = 600
		}

		ocfg := OrchestratorConfig{
			Namespace:                     ns,
			ServiceAccount:                sa,
			ImagePullSecrets:              cfg.ImagePullSecrets,
			WorkerImagePullPolicy:         cfg.WorkerImagePullPolicy,
			SidecarImagePullPolicy:        cfg.SidecarImagePullPolicy,
			JobRetention:                  retention,
			MaintenanceInterval:           maint,
			ArtifactEndpoint:              cfg.ArtifactEndpoint,
			TerminationGracePeriodSeconds: grace,
		}

		return &Orchestrator{
			client:              cs,
			namespace:           ns,
			sidecarImage:        cfg.SidecarImage,
			cfg:                 ocfg,
			retentionPeriod:     retention,
			maintenanceInterval: maint,
			emitter:             emitter,
			ctrl:                job.NewMemoryStore[kubernetesHandle](),
			watcher:             newK8sLifecycleWatcher(cs, ns),
			statusCache:         newStatusCache(),
		}, nil
	}
}

// buildRestConfig resolves a *rest.Config in this order:
//  1. explicit kubeconfig path (if set);
//  2. in-cluster config (when running as a pod);
//  3. default kubeconfig at $HOME/.kube/config.
func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

// Start reconciles pre-existing jobs and begins background maintenance.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.reconcile(ctx); err != nil {
		slog.Warn("Failed to reconcile jobs", "error", err)
	}
	maintCtx, cancel := context.WithCancel(context.Background())
	o.cancelMaintenance = cancel
	go o.runMaintenance(maintCtx, o.maintenanceInterval)
	return nil
}

// reconcile lists pre-existing batch/v1.Jobs with our managed-by label and resumes watching them.
func (o *Orchestrator) reconcile(ctx context.Context) error {
	logger := slog.With("component", "k8s.reconcile")
	jobs, err := o.client.BatchV1().Jobs(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("failed to list jobs: %w", err)
	}

	var reconciled, resumed, completed int
	for i := range jobs.Items {
		j := &jobs.Items[i]
		jobID := j.Labels[LabelJobID]
		if jobID == "" {
			continue
		}
		handle := kubernetesHandle{namespace: o.namespace, jobName: j.Name}
		reconciled++

		terminal := j.Status.Succeeded > 0 || j.Status.Failed > 0

		if terminal {
			completed++
			_ = o.ctrl.Reserve(jobID)
			o.ctrl.Commit(jobID, handle, nil)
			_ = o.ctrl.Apply(jobID, job.Started{})
			exitCode := 0
			if j.Status.Failed > 0 {
				exitCode = 1
			}
			_ = o.ctrl.Apply(jobID, job.Exited{ExitCode: exitCode})
			continue
		}

		resumed++
		watchCtx, cancelWatch := context.WithCancel(context.Background())
		cfg := watchConfigFromJob(j, handle)
		_ = o.ctrl.Reserve(jobID)
		o.ctrl.Commit(jobID, handle, cancelWatch)
		o.watchWg.Go(func() {
			o.watcher.Watch(watchCtx, cfg.namespace, cfg.jobID, func(s job.Signal) {
				_ = o.ctrl.Apply(cfg.jobID, s)
				job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
			})
		})
	}

	logger.Info("Reconciliation complete", "reconciled", reconciled, "resumed", resumed, "completed", completed)
	return nil
}

// Run creates a batch/v1.Job and starts watching its lifecycle.
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	if err := o.ctrl.Reserve(req.ID); err != nil {
		return err
	}
	h := kubernetesHandle{
		namespace: o.namespace,
		jobName:   jobNameFor(req.ID),
	}

	success := false
	defer func() {
		if !success {
			if rh, ok := o.ctrl.Release(req.ID); ok {
				o.cleanup(ctx, rh.Runtime)
			}
		}
	}()

	jobSpec := buildJob(req, o.cfg, o.sidecarImage)
	if _, err := o.client.BatchV1().Jobs(o.namespace).Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.createJob", err)
	}

	watchCtx, cancelWatch := context.WithCancel(context.Background())
	cfg := watchConfigFromRequest(req, h)
	o.ctrl.Commit(req.ID, h, cancelWatch)
	success = true
	o.watchWg.Go(func() {
		o.watcher.Watch(watchCtx, cfg.namespace, cfg.jobID, func(s job.Signal) {
			_ = o.ctrl.Apply(cfg.jobID, s)
			job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
		})
	})
	return nil
}

// Stop deletes the K8s Job (cascades to its Pod) and releases state.
func (o *Orchestrator) Stop(ctx context.Context, jobID string) error {
	h, ok := o.ctrl.Release(jobID)
	if !ok {
		return apperrors.NotFound("job", jobID)
	}
	if h.CancelWatch != nil {
		h.CancelWatch()
	}
	o.cleanup(ctx, h.Runtime)
	o.statusCache.invalidate(jobID)
	return nil
}

// Status returns the current state of a job, derived from the K8s Job (+ Pod
// when still present). Does not consult any in-process store, so all replicas
// return consistent results regardless of leader election.
// Results are cached for statusCacheTTL to absorb read bursts.
func (o *Orchestrator) Status(ctx context.Context, jobID string) (*job.StatusResponse, error) {
	if cached, ok := o.statusCache.get(jobID); ok {
		out := cached
		return &out, nil
	}

	j, err := o.client.BatchV1().Jobs(o.namespace).Get(ctx, jobNameFor(jobID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, apperrors.NotFound("job", jobID)
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.getJob", err)
	}
	status, err := deriveStatus(ctx, o.client, o.namespace, j)
	if err != nil {
		return nil, apperrors.Internal("kubernetes.deriveStatus", err)
	}
	o.statusCache.put(jobID, status)
	out := status
	return &out, nil
}

// List returns the status of all managed jobs, derived live from the K8s API.
// No cache: List is already a single paginated API call, and the response is
// a moving target that's hard to cache correctly.
func (o *Orchestrator) List(ctx context.Context) ([]job.StatusResponse, error) {
	jobs, err := o.client.BatchV1().Jobs(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listJobs", err)
	}
	statuses := make([]job.StatusResponse, 0, len(jobs.Items))
	for i := range jobs.Items {
		status, err := deriveStatus(ctx, o.client, o.namespace, &jobs.Items[i])
		if err != nil {
			slog.Warn("Failed to derive status for job", "name", jobs.Items[i].Name, "error", err)
			continue
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// Ready checks if the K8s API server is reachable.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Discovery().ServerVersion()
	return err
}

// Close stops background goroutines and waits for watchers to finish.
// Running K8s Jobs are NOT deleted — they continue independently.
func (o *Orchestrator) Close() error {
	if o.cancelMaintenance != nil {
		o.cancelMaintenance()
	}
	o.ctrl.Each(func(_ string, _ job.Entry, h job.Handle[kubernetesHandle]) {
		if h.CancelWatch != nil {
			h.CancelWatch()
		}
	})
	o.watchWg.Wait()
	return nil
}

func (o *Orchestrator) cleanup(ctx context.Context, h kubernetesHandle) {
	prop := metav1.DeletePropagationForeground
	err := o.client.BatchV1().Jobs(h.namespace).Delete(ctx, h.jobName, metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("Failed to delete K8s Job", "jobName", h.jobName, "error", err)
	}
}

func (o *Orchestrator) runMaintenance(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.cleanupExpired(ctx)
		}
	}
}

// cleanupExpired removes store entries for jobs that completed more than retentionPeriod ago.
// The K8s Job itself is GC'd by kubelet via ttlSecondsAfterFinished.
func (o *Orchestrator) cleanupExpired(ctx context.Context) {
	now := time.Now()
	var expired []string
	o.ctrl.Each(func(jobID string, e job.Entry, _ job.Handle[kubernetesHandle]) {
		if isTerminal(e.State) && now.Sub(e.UpdatedAt) > o.retentionPeriod {
			expired = append(expired, jobID)
		}
	})
	if len(expired) == 0 {
		return
	}
	for _, id := range expired {
		if h, ok := o.ctrl.Release(id); ok {
			if h.CancelWatch != nil {
				h.CancelWatch()
			}
			o.cleanup(ctx, h.Runtime)
		}
	}
}

func isTerminal(state string) bool {
	return state == job.StateCompleted || state == job.StateFailed || state == job.StateCancelled
}

// EmitArtifactEvent receives an artifact result from the sidecar and dispatches
// the corresponding CloudEvent through the orchestrator's delivery pipeline.
func (o *Orchestrator) EmitArtifactEvent(r job.ArtifactReport) {
	if r.CallbackURL == "" || !job.MatchesCallbackFilter(job.CallbackTypeArtifact, r.CallbackEvents) {
		return
	}
	builder := job.NewEventBuilder(r.JobID, "orchestrator/service", r.Meta)
	var errVal error
	if r.FailureReason != "" {
		errVal = fmt.Errorf("%s", r.FailureReason)
	}
	event := builder.BuildArtifactEvent(r.ID, r.Type, r.Status, r.Content, errVal)
	o.emitter.Emit(&job.CallbackEnvelope{
		Payload:     event,
		CallbackURL: r.CallbackURL,
		SigningKey:  r.CallbackKey,
	})
}

// Verify Orchestrator implements job.Orchestrator.
var _ job.Orchestrator = (*Orchestrator)(nil)
