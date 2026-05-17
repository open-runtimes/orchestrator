// Package kubernetes implements the job.Orchestrator interface using the Kubernetes API.
// Jobs run as batch/v1.Job resources in a configured namespace.
package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/observability"
	"orchestrator/pkg/job"
	"os"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Orchestrator implements job.Orchestrator using Kubernetes. It is HA-ready:
//
//   - Status / List / Run / Stop are stateless against the K8s API, so any
//     replica can serve any request. Dedup of concurrent Runs is handled by
//     K8s Job name uniqueness (AlreadyExists).
//   - The lifecycle watcher (which tails Pods and emits callbacks) is
//     leader-gated via a coordination.k8s.io Lease. Exactly one replica
//     watches and emits — no duplicate callbacks across replicas.
type Orchestrator struct {
	client       kubernetes.Interface
	namespace    string
	sidecarImage string
	cfg          OrchestratorConfig
	emitter      *job.CallbackEmitter
	watcher      LifecycleWatcher
	statusCache  *statusCache
	metrics      *observability.Metrics // may be nil in tests

	mu         sync.Mutex
	cancelTerm context.CancelFunc // cancels the current leadership term (or single-replica run)
	termDone   chan struct{}      // closed when the current term fully winds down
}

// Config holds configuration for the Kubernetes orchestrator.
type Config struct {
	SidecarImage                  string
	Kubeconfig                    string
	Context                       string // kubeconfig context to pin; empty uses current-context
	Namespace                     string
	ServiceAccount                string
	ImagePullSecrets              []string
	WorkerImagePullPolicy         string
	SidecarImagePullPolicy        string
	RetentionPeriod               time.Duration
	MaintenanceInterval           time.Duration
	ArtifactEndpoint              string
	TerminationGracePeriodSeconds int64
	LeaderElection                LeaderElectionConfig
	// Metrics wires backend-specific recorders (leadership, status cache,
	// tracker saturation, K8s API latency). Optional — when nil, recording
	// is skipped.
	Metrics *observability.Metrics
}

// NewOrchestrator returns an OrchestratorFactory that creates a Kubernetes orchestrator.
// Register listeners on the emitter before calling Start.
func NewOrchestrator(ctx context.Context, cfg Config) job.OrchestratorFactory {
	return func(emitter *job.CallbackEmitter) (job.Orchestrator, error) {
		restCfg, err := buildRestConfig(cfg.Kubeconfig, cfg.Context)
		if err != nil {
			return nil, fmt.Errorf("failed to build kube config: %w", err)
		}
		if cfg.Metrics != nil {
			restCfg.Wrap(newMetricsTransport(cfg.Metrics))
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

		if cfg.LeaderElection.Enabled {
			applyLeaderDefaults(&cfg.LeaderElection)
		}

		ocfg := OrchestratorConfig{
			Kubeconfig:                    cfg.Kubeconfig,
			Context:                       cfg.Context,
			Namespace:                     ns,
			ServiceAccount:                sa,
			ImagePullSecrets:              cfg.ImagePullSecrets,
			WorkerImagePullPolicy:         cfg.WorkerImagePullPolicy,
			SidecarImagePullPolicy:        cfg.SidecarImagePullPolicy,
			JobRetention:                  retention,
			ArtifactEndpoint:              cfg.ArtifactEndpoint,
			TerminationGracePeriodSeconds: grace,
			LeaderElection:                cfg.LeaderElection,
		}

		return &Orchestrator{
			client:       cs,
			namespace:    ns,
			sidecarImage: cfg.SidecarImage,
			cfg:          ocfg,
			emitter:      emitter,
			watcher:      newK8sLifecycleWatcher(cs, ns, emitter, cfg.Metrics),
			statusCache:  newStatusCache(),
			metrics:      cfg.Metrics,
		}, nil
	}
}

// applyLeaderDefaults fills in sensible defaults for leader-election timing
// and identity, matching the norms from K8s itself (15s/10s/2s).
func applyLeaderDefaults(cfg *LeaderElectionConfig) {
	if cfg.LeaseName == "" {
		cfg.LeaseName = "jobs-service-leader"
	}
	if cfg.Identity == "" {
		if hn, err := os.Hostname(); err == nil {
			cfg.Identity = hn
		} else {
			cfg.Identity = fmt.Sprintf("unknown-%d", time.Now().UnixNano())
		}
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 15 * time.Second
	}
	if cfg.RenewDeadline <= 0 {
		cfg.RenewDeadline = 10 * time.Second
	}
	if cfg.RetryPeriod <= 0 {
		cfg.RetryPeriod = 2 * time.Second
	}
}

// buildRestConfig resolves a *rest.Config in this order:
//  1. in-cluster config (when running as a pod) — only when neither kubeconfig
//     nor context is explicitly requested, so explicit overrides win;
//  2. an explicit kubeconfig path with optional context override;
//  3. default kubeconfig loading rules ($KUBECONFIG or $HOME/.kube/config)
//     with optional context override.
func buildRestConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	if kubeconfig == "" && kubeContext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

// Start begins the lifecycle watcher. With leader election enabled, only the
// elected leader runs the watcher; non-leaders block trying to acquire the
// lease. With leader election disabled (single-replica mode), the watcher
// runs unconditionally in the background.
//
// HTTP API handlers (Run/Stop/Status/List) remain active on all replicas
// regardless of leadership state — Kubernetes is the source of truth.
func (o *Orchestrator) Start(ctx context.Context) error {
	termCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	o.mu.Lock()
	o.cancelTerm = cancel
	o.termDone = done
	o.mu.Unlock()

	go func() {
		defer close(done)
		if o.cfg.LeaderElection.Enabled {
			o.runLeaderElection(termCtx)
			return
		}
		// Single-replica mode: this process is effectively always the leader,
		// so report it as such. identity is the hostname (or pod name) so the
		// dashboard panel still has a label to display.
		identity, _ := os.Hostname()
		if identity == "" {
			identity = "single-replica"
		}
		if o.metrics != nil {
			o.metrics.RecordLeadership(termCtx, identity, true)
		}
		o.watcher.Start(termCtx)
		if o.metrics != nil {
			o.metrics.RecordLeadership(context.Background(), identity, false)
		}
	}()
	return nil
}

// runLeaderElection loops so that if RunOrDie returns (lease lost or ctx
// cancelled but not released), we retry until ctx is truly cancelled. The
// inner RunOrDie call drives watcher.Start via OnStartedLeading.
func (o *Orchestrator) runLeaderElection(ctx context.Context) {
	logger := slog.With("component", "k8s.leaderelection", "identity", o.cfg.LeaderElection.Identity)
	for {
		if ctx.Err() != nil {
			return
		}
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      o.cfg.LeaderElection.LeaseName,
				Namespace: o.namespace,
			},
			Client: o.client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: o.cfg.LeaderElection.Identity,
			},
		}
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			ReleaseOnCancel: true,
			LeaseDuration:   o.cfg.LeaderElection.LeaseDuration,
			RenewDeadline:   o.cfg.LeaderElection.RenewDeadline,
			RetryPeriod:     o.cfg.LeaderElection.RetryPeriod,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leaderCtx context.Context) {
					logger.Info("Acquired leadership; starting watcher")
					if o.metrics != nil {
						o.metrics.RecordLeadership(leaderCtx, o.cfg.LeaderElection.Identity, true)
					}
					o.watcher.Start(leaderCtx)
					logger.Info("Watcher stopped; leadership term ended")
				},
				OnStoppedLeading: func() {
					logger.Info("Lost leadership")
					if o.metrics != nil {
						o.metrics.RecordLeadership(context.Background(), o.cfg.LeaderElection.Identity, false)
					}
				},
				OnNewLeader: func(identity string) {
					if identity != o.cfg.LeaderElection.Identity {
						logger.Info("New leader elected", "leader", identity)
					}
				},
			},
		})
	}
}

// Run creates a batch/v1.Job. Dedup on concurrent creates is enforced by K8s
// name uniqueness: two replicas racing to create the same jobID will see one
// succeed and the other receive an AlreadyExists translated to a Conflict.
// The job's watcher will be spawned automatically by the leader's informer
// when K8s creates the owning Pod.
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	jobSpec := buildJob(req, o.cfg, o.sidecarImage)
	if _, err := o.client.BatchV1().Jobs(o.namespace).Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return apperrors.Conflict("job", req.ID, "job already exists")
		}
		return apperrors.Internal("kubernetes.createJob", err)
	}
	return nil
}

// Stop deletes the K8s Job (cascades to its Pod). Idempotent at the K8s layer;
// a second call against a non-existent Job returns our NotFound error. The
// leader's watcher observes the Pod deletion and tears down its tracker.
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
	o.statusCache.invalidate(jobID)
	return nil
}

// Status returns the current state of a job, derived from the K8s Job (+ Pod
// when still present). Does not consult any in-process store, so all replicas
// return consistent results regardless of leader election.
// Results are cached for statusCacheTTL to absorb read bursts.
func (o *Orchestrator) Status(ctx context.Context, jobID string) (*job.StatusResponse, error) {
	if cached, ok := o.statusCache.get(jobID); ok {
		if o.metrics != nil {
			o.metrics.RecordStatusCacheHit(ctx)
		}
		out := cached
		return &out, nil
	}
	if o.metrics != nil {
		o.metrics.RecordStatusCacheMiss(ctx)
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

// Close cancels the leader-election loop (or single-replica watcher) and
// waits for it to wind down. Running K8s Jobs are NOT deleted — they
// continue independently and will be picked up by the next orchestrator
// start via the watcher's informer.
func (o *Orchestrator) Close() error {
	o.mu.Lock()
	cancel := o.cancelTerm
	done := o.termDone
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
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
		Headers:     r.CallbackHeaders,
	})
}

// Verify Orchestrator implements job.Orchestrator.
var _ job.Orchestrator = (*Orchestrator)(nil)
