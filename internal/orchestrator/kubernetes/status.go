package kubernetes

import (
	"context"
	"fmt"
	"orchestrator/pkg/job"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// statusCacheTTL is how long a derived StatusResponse is reused for the same
// jobID before we re-query the K8s API. Short enough that transient hot-path
// reads can't linger on stale data, long enough to absorb request bursts.
const statusCacheTTL = 5 * time.Second

// statusCache is a bounded-lifetime cache for derived Status responses. All
// entries expire after statusCacheTTL; stale entries are lazily replaced on
// the next read. Suitable because the TTL is short and entries are small.
type statusCache struct {
	mu      sync.Mutex
	entries map[string]statusCacheEntry
}

type statusCacheEntry struct {
	expiry time.Time
	status job.StatusResponse
}

func newStatusCache() *statusCache {
	return &statusCache{entries: make(map[string]statusCacheEntry)}
}

func (c *statusCache) get(jobID string) (job.StatusResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[jobID]
	if !ok || time.Now().After(e.expiry) {
		return job.StatusResponse{}, false
	}
	return e.status, true
}

func (c *statusCache) put(jobID string, status job.StatusResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[jobID] = statusCacheEntry{
		expiry: time.Now().Add(statusCacheTTL),
		status: status,
	}
}

func (c *statusCache) invalidate(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, jobID)
}

// deriveStatus converts a batch/v1.Job (plus its Pod, when reachable) into
// the backend-agnostic StatusResponse. K8s is the source of truth — no in-memory
// store is consulted, so this works correctly on any replica regardless of
// leader election.
//
// Mapping:
//   - Job.Status.Succeeded > 0 → Completed, exit code 0
//   - Job.Status.Failed > 0    → Failed, exit code + reason from Pod (when Pod still exists)
//   - Job.Status.Active > 0, worker container Running → Running
//   - otherwise                → Accepted (pending scheduler / init / worker start)
func deriveStatus(ctx context.Context, client kubernetes.Interface, namespace string, j *batchv1.Job) (job.StatusResponse, error) {
	jobID := j.Labels[LabelJobID]
	if jobID == "" {
		return job.StatusResponse{}, fmt.Errorf("job %s missing %s label", j.Name, LabelJobID)
	}
	resp := job.StatusResponse{ID: jobID}

	if j.Status.Succeeded > 0 {
		resp.State = job.StateCompleted
		zero := 0
		resp.ExitCode = &zero
		return resp, nil
	}

	if j.Status.Failed > 0 {
		resp.State = job.StateFailed
		annotateFailure(ctx, client, namespace, jobID, &resp)
		return resp, nil
	}

	// Non-terminal. Differentiate Running from Accepted by looking at the Pod.
	pod := fetchPodForJob(ctx, client, namespace, jobID)
	if pod != nil {
		if worker := findWorkerStatus(pod); worker != nil && worker.State.Running != nil {
			resp.State = job.StateRunning
			return resp, nil
		}
	}
	resp.State = job.StateAccepted
	return resp, nil
}

// annotateFailure fills in ExitCode and Error on a Failed status from the Pod,
// when the Pod is still present. Best-effort: if the Pod has been garbage
// collected (TTL elapsed), status stays minimal.
func annotateFailure(ctx context.Context, client kubernetes.Interface, namespace, jobID string, resp *job.StatusResponse) {
	pod := fetchPodForJob(ctx, client, namespace, jobID)
	if pod == nil {
		return
	}
	worker := findWorkerStatus(pod)
	if worker == nil || worker.State.Terminated == nil {
		if pod.Status.Reason != "" {
			resp.Error = pod.Status.Reason
		}
		return
	}
	code := int(worker.State.Terminated.ExitCode)
	resp.ExitCode = &code
	switch {
	case worker.State.Terminated.Message != "":
		resp.Error = worker.State.Terminated.Message
	case worker.State.Terminated.Reason != "":
		resp.Error = worker.State.Terminated.Reason
	}
}

// fetchPodForJob returns the first Pod carrying the job.id label in the given
// namespace, or nil if none exists. The K8s Job controller only ever creates
// one Pod per Job in our setup (parallelism=1, backoffLimit=0, restartPolicy=Never).
func fetchPodForJob(ctx context.Context, client kubernetes.Interface, namespace, jobID string) *corev1.Pod {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelJobID + "=" + jobID,
	})
	if err != nil || len(pods.Items) == 0 {
		return nil
	}
	return &pods.Items[0]
}

func findWorkerStatus(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == ContainerWorker {
			return cs
		}
	}
	return nil
}
