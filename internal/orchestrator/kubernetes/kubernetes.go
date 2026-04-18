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
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Orchestrator implements job.Orchestrator using Kubernetes. No in-memory
// state is authoritative: Status/List derive from the K8s API and dedup of
// concurrent Run calls is enforced by K8s name uniqueness. The only local
// state is a map of active watcher cancellation functions used so Close and
// Stop can tear watchers down — that map is not consulted for correctness.
type Orchestrator struct {
	client       kubernetes.Interface
	namespace    string
	sidecarImage string
	cfg          OrchestratorConfig
	emitter      *job.CallbackEmitter
	watcher      LifecycleWatcher
	statusCache  *statusCache

	watchersMu   sync.Mutex
	watchers     map[string]*watcherEntry
	watcherIDGen atomic.Uint64
	watchWg      sync.WaitGroup
}

// watcherEntry pairs a context cancel with a unique id so that concurrent
// spawnWatcher/stopWatcher calls for the same jobID can tell whether a map
// entry still belongs to them when tearing down on goroutine exit.
type watcherEntry struct {
	cancel context.CancelFunc
	id     uint64
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
		retention := cfg.RetentionPeriod
		if retention <= 0 {
			retention = 15 * time.Minute
		}

		ocfg := OrchestratorConfig{
			Namespace:                     ns,
			ServiceAccount:                sa,
			ImagePullSecrets:              cfg.ImagePullSecrets,
			WorkerImagePullPolicy:         cfg.WorkerImagePullPolicy,
			SidecarImagePullPolicy:        cfg.SidecarImagePullPolicy,
			JobRetention:                  retention,
			ArtifactEndpoint:              cfg.ArtifactEndpoint,
			TerminationGracePeriodSeconds: grace,
		}

		return &Orchestrator{
			client:       cs,
			namespace:    ns,
			sidecarImage: cfg.SidecarImage,
			cfg:          ocfg,
			emitter:      emitter,
			watcher:      newK8sLifecycleWatcher(cs, ns),
			statusCache:  newStatusCache(),
			watchers:     make(map[string]*watcherEntry),
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

// Start resumes watching any pre-existing non-terminal Jobs so callbacks keep
// flowing after a service restart. There's no maintenance loop: K8s
// ttlSecondsAfterFinished on the Job spec handles terminal-Job GC centrally.
func (o *Orchestrator) Start(ctx context.Context) error {
	if err := o.reconcile(ctx); err != nil {
		slog.Warn("Failed to reconcile jobs", "error", err)
	}
	return nil
}

// reconcile lists existing managed Jobs and spawns a watcher for each
// non-terminal one so lifecycle callbacks resume after a service restart.
func (o *Orchestrator) reconcile(ctx context.Context) error {
	logger := slog.With("component", "k8s.reconcile")
	jobs, err := o.client.BatchV1().Jobs(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return fmt.Errorf("failed to list jobs: %w", err)
	}

	var resumed int
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Status.Succeeded > 0 || j.Status.Failed > 0 {
			continue
		}
		jobID := j.Labels[LabelJobID]
		if jobID == "" {
			continue
		}
		o.spawnWatcher(watchConfigFromJob(j))
		resumed++
	}

	logger.Info("Reconciliation complete", "resumed", resumed, "total", len(jobs.Items))
	return nil
}

// Run creates a batch/v1.Job and spawns a local lifecycle watcher. Dedup on
// concurrent creates is enforced by K8s name uniqueness: two replicas racing
// to create the same jobID will see one succeed and the other receive an
// AlreadyExists which we translate into a Conflict error.
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	jobSpec := buildJob(req, o.cfg, o.sidecarImage)
	if _, err := o.client.BatchV1().Jobs(o.namespace).Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return apperrors.Conflict("job", req.ID, "job already exists")
		}
		return apperrors.Internal("kubernetes.createJob", err)
	}
	o.spawnWatcher(watchConfigFromRequest(req))
	return nil
}

// Stop deletes the K8s Job (cascades to its Pod). Idempotent at the K8s layer;
// a second call against a non-existent Job returns our NotFound error.
func (o *Orchestrator) Stop(ctx context.Context, jobID string) error {
	prop := metav1.DeletePropagationForeground
	err := o.client.BatchV1().Jobs(o.namespace).Delete(ctx, jobNameFor(jobID), metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if apierrors.IsNotFound(err) {
		return apperrors.NotFound("job", jobID)
	}
	if err != nil {
		return apperrors.Internal("kubernetes.deleteJob", err)
	}
	o.cancelWatcher(jobID)
	o.statusCache.invalidate(jobID)
	return nil
}

// spawnWatcher registers and starts a watcher goroutine for jobID. If a
// watcher is already running for this jobID (e.g. resume after reconcile
// followed by Run), the previous one is cancelled first.
func (o *Orchestrator) spawnWatcher(cfg *watchConfig) {
	watchCtx, cancel := context.WithCancel(context.Background())
	id := o.watcherIDGen.Add(1)
	entry := &watcherEntry{cancel: cancel, id: id}

	o.watchersMu.Lock()
	if prev, ok := o.watchers[cfg.jobID]; ok {
		prev.cancel()
	}
	o.watchers[cfg.jobID] = entry
	o.watchersMu.Unlock()

	o.watchWg.Go(func() {
		defer func() {
			o.watchersMu.Lock()
			if cur, ok := o.watchers[cfg.jobID]; ok && cur.id == id {
				delete(o.watchers, cfg.jobID)
			}
			o.watchersMu.Unlock()
		}()
		o.watcher.Watch(watchCtx, o.namespace, cfg.jobID, func(s job.Signal) {
			job.EmitCallback(o.emitter, cfg.jobID, cfg.image, cfg.dest, s)
		})
	})
}

// cancelWatcher stops the watcher for jobID if one is running. No-op otherwise.
func (o *Orchestrator) cancelWatcher(jobID string) {
	o.watchersMu.Lock()
	defer o.watchersMu.Unlock()
	if e, ok := o.watchers[jobID]; ok {
		e.cancel()
		delete(o.watchers, jobID)
	}
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

// Close cancels all in-flight watchers and waits for their goroutines to exit.
// Running K8s Jobs are NOT deleted — they continue independently and will be
// picked up by the next orchestrator start via reconcile().
func (o *Orchestrator) Close() error {
	o.watchersMu.Lock()
	for _, e := range o.watchers {
		e.cancel()
	}
	o.watchers = make(map[string]*watcherEntry)
	o.watchersMu.Unlock()
	o.watchWg.Wait()
	return nil
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
