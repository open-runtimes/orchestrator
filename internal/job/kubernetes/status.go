package kubernetes

import (
	"context"
	"fmt"
	"orchestrator/internal/job"
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
			annotateFailure(ctx, client, namespace, jobID, &resp)
			return resp, nil
		}
	}
	if j.Status.Failed > 0 {
		resp.State = job.StateFailed
		annotateFailure(ctx, client, namespace, jobID, &resp)
		return resp, nil
	}

	// The Job is not Succeeded or Failed yet, but the worker may already have
	// exited: a Job counts its Pod as done only once EVERY container has
	// stopped, and the native sidecar keeps running to process post-job
	// artifacts. So ask the Pod what the worker did.
	pod := fetchPodForJob(ctx, client, namespace, jobID)
	if pod != nil {
		if worker := findWorkerStatus(pod); worker != nil {
			switch {
			case worker.State.Terminated != nil:
				// The exit is the answer. Reporting Accepted here would move the
				// state backwards from Running, and contradict both the callback
				// already emitted for this exit and the Docker backend.
				resp.State = job.StateForExit(int(worker.State.Terminated.ExitCode))
				annotateTermination(worker, &resp)
				if gatedPod(pod) && !workloadReleased(pod, worker) {
					resp.State = job.StateFailed
					resp.Error = "workload startup gate failed before command execution"
				}
				return resp, nil
			case worker.State.Running != nil && workloadReleased(pod, worker):
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
	case term.Message != "" && executionStart(worker).IsZero():
		resp.Error = term.Message
	case term.Reason != "":
		resp.Error = term.Reason
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
