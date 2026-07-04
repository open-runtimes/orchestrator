package kubernetes

import (
	"orchestrator/pkg/deployment"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// progressDeadlineExceeded is the Progressing condition reason set by the
// deployment controller when spec.progressDeadlineSeconds elapses without
// progress. Not exported by k8s.io/api.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

// deriveStatus converts an apps/v1.Deployment into the backend-agnostic
// StatusResponse. Kubernetes is the source of truth — no in-process state is
// consulted, so all replicas derive identical results.
//
// Mapping:
//   - DeletionTimestamp set        → deleting
//   - available >= desired         → ready
//   - 0 < available < desired      → degraded
//   - available == 0: failed when the controller reports ProgressDeadlineExceeded
//     or a replica failure (condition message becomes Error); otherwise pending
func deriveStatus(dep *appsv1.Deployment) deployment.StatusResponse {
	desired := 1
	if dep.Spec.Replicas != nil {
		desired = int(*dep.Spec.Replicas)
	}
	available := int(dep.Status.AvailableReplicas)

	resp := deployment.StatusResponse{
		ID:                dep.Labels[LabelDeploymentID],
		DesiredReplicas:   desired,
		AvailableReplicas: available,
	}

	switch {
	case dep.DeletionTimestamp != nil:
		resp.State = deployment.StateDeleting
	case available >= desired:
		resp.State = deployment.StateReady
	case available > 0:
		resp.State = deployment.StateDegraded
	default:
		if msg, failed := rolloutFailure(dep); failed {
			resp.State = deployment.StateFailed
			resp.Error = msg
		} else {
			resp.State = deployment.StatePending
		}
	}
	return resp
}

// rolloutFailure reports whether the deployment controller has given up on the
// rollout, with the condition message explaining why.
func rolloutFailure(dep *appsv1.Deployment) (string, bool) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == progressDeadlineExceeded {
			return c.Message, true
		}
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return c.Message, true
		}
	}
	return "", false
}
