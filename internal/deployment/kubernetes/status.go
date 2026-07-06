package kubernetes

import (
	"cmp"
	"context"
	"log/slog"
	"net"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"slices"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// progressDeadlineExceeded is the Progressing condition reason set by the
// deployment controller when spec.readyTimeoutSeconds elapses without
// progress. Not exported by k8s.io/api.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

// Status returns the deployment's current state, aggregated marker-first:
// revisions from the existing revision Deployments, the traffic table from
// the route, replica counts summed over the traffic-weighted revisions.
func (o *Orchestrator) Status(ctx context.Context, id string) (*deployment.StatusResponse, error) {
	m, err := o.getMarker(ctx, id)
	if err != nil {
		return nil, err
	}
	return o.deriveStatus(ctx, m)
}

// List returns the status of all managed deployments — one per marker.
func (o *Orchestrator) List(ctx context.Context) ([]deployment.StatusResponse, error) {
	markers, err := o.client.CoreV1().ConfigMaps(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listMarkers", err)
	}
	statuses := make([]deployment.StatusResponse, 0, len(markers.Items))
	for i := range markers.Items {
		m := markerFromConfigMap(&markers.Items[i])
		if m.ID == "" {
			slog.Warn("Skipping managed marker without a deployment.id label", "name", markers.Items[i].Name)
			continue
		}
		status, err := o.deriveStatus(ctx, m)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, *status)
	}
	return statuses, nil
}

// Endpoints returns the proxy data-port URL of every ready pod belonging to
// a traffic-weighted revision — the activator's direct forward targets.
func (o *Orchestrator) Endpoints(ctx context.Context, id string) ([]*url.URL, error) {
	m, err := o.getMarker(ctx, id)
	if err != nil {
		return nil, err
	}
	routed := routedSet(o.currentTargets(ctx, m))

	pods, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelDeploymentID + "=" + id,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listPods", err)
	}
	var endpoints []*url.URL
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !routed[pod.Labels[LabelRevision]] || !isPodReady(pod) || pod.Status.PodIP == "" {
			continue
		}
		endpoints = append(endpoints, &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(proxy.DefaultProxyPort)),
		})
	}
	return endpoints, nil
}

// deriveStatus assembles the StatusResponse for one marker.
func (o *Orchestrator) deriveStatus(ctx context.Context, m marker) (*deployment.StatusResponse, error) {
	deps, err := o.revisionDeployments(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	targets := o.currentTargets(ctx, m)

	mode := m.TrafficMode
	if mode == "" {
		mode = deployment.ModeAuto
	}
	resp := deployment.StatusResponse{
		ID:        m.ID,
		Revisions: make([]string, 0, len(deps)),
		Traffic:   targets,
		Mode:      mode,
	}
	byRevision := make(map[string]*appsv1.Deployment, len(deps))
	for i := range deps {
		rev := deps[i].Labels[LabelRevision]
		resp.Revisions = append(resp.Revisions, rev)
		byRevision[rev] = &deps[i]
	}

	routed := routedSet(targets)
	for rev := range routed {
		if dep := byRevision[rev]; dep != nil {
			resp.DesiredReplicas += desiredReplicas(dep)
			resp.AvailableReplicas += int(dep.Status.AvailableReplicas)
		}
	}

	resp.State, resp.Error = deriveState(m, resp.DesiredReplicas, resp.AvailableReplicas, byRevision, routed)
	return &resp, nil
}

// deriveState maps the aggregate onto the backend-agnostic states:
//
//   - marker deleting                 → deleting
//   - all routed desired == 0         → idle (scaled to zero)
//   - head failed before ever cutting → failed — traffic still flows to the
//     old revision, but a stuck rollout must be visible (latest ≠ lastReady)
//   - available >= desired            → ready
//   - some available                  → degraded
//   - none available: failed when a routed revision's controller gave up
//     (deadline exceeded / replica failure), else pending
func deriveState(m marker, desired, available int, byRevision map[string]*appsv1.Deployment, routed map[string]bool) (state, message string) {
	switch {
	case m.Deleting:
		return deployment.StateDeleting, ""
	case desired == 0:
		return deployment.StateIdle, ""
	}
	if m.LatestRevision != m.LastReady {
		if dep := byRevision[m.LatestRevision]; dep != nil {
			if msg, failed := rolloutFailure(dep); failed {
				return deployment.StateFailed, msg
			}
		}
	}
	switch {
	case available >= desired:
		return deployment.StateReady, ""
	case available > 0:
		return deployment.StateDegraded, ""
	}
	for rev := range routed {
		if dep := byRevision[rev]; dep != nil {
			if msg, failed := rolloutFailure(dep); failed {
				return deployment.StateFailed, msg
			}
		}
	}
	return deployment.StatePending, ""
}

// revisionDeployments lists the deployment's revision Deployments, newest
// first.
func (o *Orchestrator) revisionDeployments(ctx context.Context, id string) ([]appsv1.Deployment, error) {
	deps, err := o.client.AppsV1().Deployments(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue + "," + LabelDeploymentID + "=" + id,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listRevisions", err)
	}
	slices.SortFunc(deps.Items, func(a, b appsv1.Deployment) int {
		return cmp.Compare(revisionNumber(b.Labels[LabelRevision]), revisionNumber(a.Labels[LabelRevision]))
	})
	return deps.Items, nil
}

// revisionNames returns every revision name that still has a Deployment or a
// Service, newest first — the teardown and retire inventory.
func (o *Orchestrator) revisionNames(ctx context.Context, id string) ([]string, error) {
	deps, err := o.revisionDeployments(ctx, id)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(deps))
	names := make([]string, 0, len(deps))
	for i := range deps {
		rev := deps[i].Labels[LabelRevision]
		seen[rev] = true
		names = append(names, rev)
	}

	svcs, err := o.client.CoreV1().Services(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue + "," + LabelDeploymentID + "=" + id,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listServices", err)
	}
	for i := range svcs.Items {
		if rev := svcs.Items[i].Labels[LabelRevision]; rev != "" && !seen[rev] {
			names = append(names, rev)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return cmp.Compare(revisionNumber(b), revisionNumber(a)) })
	return names, nil
}

// routedSet is the set of revisions carrying weight in a traffic table.
func routedSet(targets []deployment.Target) map[string]bool {
	set := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t.Percent > 0 {
			set[t.RevisionName] = true
		}
	}
	return set
}

func desiredReplicas(dep *appsv1.Deployment) int {
	if dep.Spec.Replicas != nil {
		return int(*dep.Spec.Replicas)
	}
	return 1
}

// rolloutFailure reports whether the deployment controller has given up on
// the revision, with the condition message explaining why.
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

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
