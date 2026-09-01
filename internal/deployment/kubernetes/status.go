package kubernetes

import (
	"cmp"
	"context"
	"log/slog"
	"net"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/deployment"
	revisionapi "orchestrator/internal/revision"
	"orchestrator/internal/workload"
	"slices"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// progressDeadlineExceeded is the Ready condition reason set by the direct-pod
// controller when readyTimeoutSeconds elapses without progress.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

// Status returns the deployment's current state, aggregated marker-first:
// revisions from the existing Revision CRs, the traffic table from
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
		if !routed[pod.Labels[LabelRevision]] || !podReadyForRevision(pod) || pod.Status.PodIP == "" {
			continue
		}
		endpoints = append(endpoints, &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(workload.DefaultProxyPort)),
		})
	}
	return endpoints, nil
}

// deriveStatus assembles the StatusResponse for one marker.
func (o *Orchestrator) deriveStatus(ctx context.Context, m marker) (*deployment.StatusResponse, error) {
	revisions, err := o.revisionResources(ctx, m.ID)
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
		Revisions: make([]string, 0, len(revisions)),
		Traffic:   targets,
		Mode:      mode,
	}
	byRevision := make(map[string]*revisionapi.Revision, len(revisions))
	for i := range revisions {
		rev := revisions[i].Labels[LabelRevision]
		resp.Revisions = append(resp.Revisions, rev)
		byRevision[rev] = &revisions[i]
	}

	routed := routedSet(targets)
	for rev := range routed {
		if revision := byRevision[rev]; revision != nil {
			resp.DesiredReplicas += int(revision.Spec.Replicas)
			resp.AvailableReplicas += int(revision.Status.ReadyReplicas)
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
func deriveState(m marker, desired, available int, byRevision map[string]*revisionapi.Revision, routed map[string]bool) (state, message string) {
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

// revisionResources lists the deployment's Revisions, newest
// first.
func (o *Orchestrator) revisionResources(ctx context.Context, id string) ([]revisionapi.Revision, error) {
	revisions, err := o.revisions.List(ctx, o.namespace, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue + "," + LabelDeploymentID + "=" + id,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listRevisions", err)
	}
	slices.SortFunc(revisions.Items, func(a, b revisionapi.Revision) int {
		return cmp.Compare(revisionNumber(b.Labels[LabelRevision]), revisionNumber(a.Labels[LabelRevision]))
	})
	return revisions.Items, nil
}

// revisionNames returns every revision name that still has a Revision or a
// Service, newest first — the teardown and retire inventory.
func (o *Orchestrator) revisionNames(ctx context.Context, id string) ([]string, error) {
	revisions, err := o.revisionResources(ctx, id)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(revisions))
	names := make([]string, 0, len(revisions))
	for i := range revisions {
		rev := revisions[i].Labels[LabelRevision]
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

// rolloutFailure reports whether the revision controller has given up waiting
// for readiness, with the condition message explaining why.
func rolloutFailure(revision *revisionapi.Revision) (string, bool) {
	for _, c := range revision.Status.Conditions {
		if c.Type == revisionConditionReady && c.Status == metav1.ConditionFalse {
			return c.Message, true
		}
	}
	return "", false
}
