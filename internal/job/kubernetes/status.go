package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"orchestrator/internal/job"
	"orchestrator/internal/lifecycle"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// errNoPod reports that a Job has no Pod: not yet created, or already
// garbage collected.
var errNoPod = errors.New("job has no pod")

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
//   - worker container terminated → job.StateForExit, the same rule the store
//     applies to an Exited signal, so an API read and the callback describing
//     that exit agree, and so does the Docker backend
//   - worker container Running → Running
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

	// A deadline can remove the pod; retain the controller's failure reason
	// so setup retries do not end in an unexplained failure response.
	for _, condition := range j.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			resp.State = job.StateFailed
			resp.Error = condition.Reason
			if err := annotateFailure(ctx, client, namespace, jobID, &resp); err != nil {
				return job.StatusResponse{}, err
			}
			return resp, nil
		}
	}
	if j.Status.Failed > 0 {
		resp.State = job.StateFailed
		if err := annotateFailure(ctx, client, namespace, jobID, &resp); err != nil {
			return job.StatusResponse{}, err
		}
		return resp, nil
	}

	// The Job is not Succeeded or Failed yet, but the worker may already have
	// exited: a Job counts its Pod as done only once EVERY container has
	// stopped, and the native sidecar keeps running to process post-job
	// artifacts. So ask the Pod what the worker did.
	pod, err := fetchPodForJob(ctx, client, namespace, jobID)
	if cutShort(ctx, err) {
		return job.StatusResponse{}, err
	}
	if err == nil {
		if worker := findWorkerStatus(pod); worker != nil {
			switch {
			case worker.State.Terminated != nil:
				// The exit is the answer. Reporting Accepted here would move the
				// state backwards from Running, and contradict both the callback
				// already emitted for this exit and the Docker backend.
				resp.State = lifecycle.StateForExit(int(worker.State.Terminated.ExitCode))
				annotateTermination(worker, &resp)
				return resp, nil
			case worker.State.Running != nil:
				resp.State = job.StateRunning
				return resp, nil
			}
		}
	}
	resp.State = job.StateAccepted
	return resp, nil
}

// annotateFailure fills in ExitCode and Error on a Failed status from the Pod,
// when the Pod is still present. Best-effort: if the Pod has been garbage
// collected (TTL elapsed) or cannot be read, status stays minimal; only a
// lookup the request's deadline or cancellation cut short is an error.
func annotateFailure(ctx context.Context, client kubernetes.Interface, namespace, jobID string, resp *job.StatusResponse) error {
	pod, err := fetchPodForJob(ctx, client, namespace, jobID)
	if cutShort(ctx, err) {
		return err
	}
	if err == nil {
		annotatePod(pod, resp)
	}
	return nil
}

func annotatePod(pod *corev1.Pod, resp *job.StatusResponse) {
	worker := findWorkerStatus(pod)
	if worker == nil || worker.State.Terminated == nil {
		if pod.Status.Reason != "" {
			resp.Error = pod.Status.Reason
		}
		return
	}
	annotateTermination(worker, resp)
}

// annotateTermination copies a terminated worker's exit code, and what the
// kubelet said about it, onto the response.
func annotateTermination(worker *corev1.ContainerStatus, resp *job.StatusResponse) {
	term := worker.State.Terminated
	code := int(term.ExitCode)
	resp.ExitCode = &code
	if code == 0 {
		// A clean exit carries Reason "Completed", which is not an error.
		return
	}
	switch {
	case term.Message != "":
		resp.Error = term.Message
	case term.Reason != "":
		resp.Error = term.Reason
	}
}

// fetchPodForJob returns the first Pod carrying the job.id label in the given
// namespace, or errNoPod if none exists. The K8s Job controller only ever creates
// one Pod per Job in our setup (parallelism=1, backoffLimit=0, restartPolicy=Never).
func fetchPodForJob(ctx context.Context, client kubernetes.Interface, namespace, jobID string) (*corev1.Pod, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelJobID + "=" + jobID,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, errNoPod
	}
	return &pods.Items[0], nil
}

// cutShort reports whether a pod lookup failed because the request ran out of
// time or was abandoned. Answering that with the Job alone would regress a
// running or exited job to Accepted, and Status would cache the regression.
func cutShort(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
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
