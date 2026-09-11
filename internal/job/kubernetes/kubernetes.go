// Package kubernetes implements the job.Orchestrator interface using the Kubernetes API.
// Jobs run as batch/v1.Job resources in a configured namespace.
package kubernetes

import (
	"context"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/job"
	"orchestrator/internal/kube"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
	client      kubernetes.Interface
	cfg         Config
	emitter     *job.CallbackEmitter
	watcher     LifecycleWatcher
	statusCache *statusCache

	mu         sync.Mutex
	cancelTerm context.CancelFunc // cancels the current leadership term (or single-replica run)
	termDone   chan struct{}      // closed when the current term fully winds down
}

// NewOrchestrator creates a Kubernetes orchestrator. Register listeners on
// the emitter before calling Start.
func NewOrchestrator(cfg Config, emitter *job.CallbackEmitter) (*Orchestrator, error) {
	cs, err := kube.NewClient(cfg.Kubeconfig, cfg.Context, cfg.Metrics)
	if err != nil {
		return nil, err
	}
	o := &Orchestrator{
		client:      cs,
		cfg:         cfg,
		emitter:     emitter,
		watcher:     newK8sLifecycleWatcher(cs, cfg.Namespace, emitter, cfg.LogFlushInterval),
		statusCache: newStatusCache(),
	}
	if err := cfg.Metrics.ObserveInt64("orchestrator_trackers",
		"In-flight per-job lifecycle trackers on the leader (saturation)",
		func() int64 { trackers, _ := o.watcher.Counts(); return trackers },
	); err != nil {
		return nil, err
	}
	return o, nil
}

// ActiveJobs reports the jobs this replica is watching that have not exited.
// Only the leader holds trackers, so followers report zero and the sum across
// replicas is the true in-flight count.
func (o *Orchestrator) ActiveJobs() int64 {
	_, active := o.watcher.Counts()
	return active
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
		kube.RunLeaderElected(termCtx, o.client, o.cfg.Namespace, o.cfg.LeaderElection, o.watcher.Start, o.cfg.Metrics.RecordLeadership)
	}()
	return nil
}

// Run creates a batch/v1.Job. Dedup on concurrent creates is enforced by K8s
// name uniqueness: two replicas racing to create the same jobID will see one
// succeed and the other receive an AlreadyExists translated to a Conflict.
// The job's watcher will be spawned automatically by the leader's informer
// when K8s creates the owning Pod.
func (o *Orchestrator) Run(ctx context.Context, req *job.Request) error {
	jobSpec := buildJob(req, o.cfg, o.cfg.SidecarImage)
	if _, err := o.client.BatchV1().Jobs(o.cfg.Namespace).Create(ctx, jobSpec, metav1.CreateOptions{}); err != nil {
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
	err := o.client.BatchV1().Jobs(o.cfg.Namespace).Delete(ctx, jobNameFor(jobID), metav1.DeleteOptions{
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
		o.cfg.Metrics.RecordStatusCacheHit(ctx)
		out := cached
		return &out, nil
	}
	o.cfg.Metrics.RecordStatusCacheMiss(ctx)

	j, err := o.client.BatchV1().Jobs(o.cfg.Namespace).Get(ctx, jobNameFor(jobID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, apperrors.NotFound("job", jobID)
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.getJob", err)
	}
	status, err := deriveStatus(ctx, o.client, o.cfg.Namespace, j)
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
	jobs, err := o.client.BatchV1().Jobs(o.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listJobs", err)
	}
	statuses := make([]job.StatusResponse, 0, len(jobs.Items))
	for i := range jobs.Items {
		status, err := deriveStatus(ctx, o.client, o.cfg.Namespace, &jobs.Items[i])
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

// Verify Orchestrator implements job.Orchestrator.
var _ job.Orchestrator = (*Orchestrator)(nil)
