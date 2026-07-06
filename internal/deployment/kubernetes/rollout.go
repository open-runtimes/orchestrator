package kubernetes

import (
	"context"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// rolloutInterval is how often the rollout reconciler sweeps the markers.
const rolloutInterval = 2 * time.Second

// runRollouts drives the auto-cut loop until the context is cancelled
// (Close). Kubernetes-native readiness is the trigger: a freshly minted
// revision receives traffic only once its Deployment reports an available
// replica — a failed new revision never receives traffic, and rollback stays
// a manual traffic edit (failure-semantics: no auto-rollback).
func (o *Orchestrator) runRollouts(ctx context.Context) {
	ticker := time.NewTicker(rolloutInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.reconcileRollouts(ctx)
		}
	}
}

// reconcileRollouts sweeps every marker and advances pending rollouts.
func (o *Orchestrator) reconcileRollouts(ctx context.Context) {
	markers, err := o.client.CoreV1().ConfigMaps(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		slog.Warn("Rollout sweep failed to list markers", "error", err)
		return
	}
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.RecordDeploymentsActive(ctx, int64(len(markers.Items)))
	}
	for i := range markers.Items {
		m := markerFromConfigMap(&markers.Items[i])
		if m.ID == "" {
			continue
		}
		if err := o.reconcileRollout(ctx, m); err != nil {
			slog.Warn("Rollout reconcile failed", "deploymentId", m.ID, "error", err)
		}
	}
}

// reconcileRollout performs one rollout evaluation for a deployment: when the
// mode is auto and the head revision isn't the last-ready one, cut 100% of
// traffic to it once it has an available replica, record it as last-ready,
// and retire surplus revisions. An unavailable or failed head is left alone —
// traffic keeps flowing to the old revision.
func (o *Orchestrator) reconcileRollout(ctx context.Context, m marker) error {
	if m.Deleting || m.LatestRevision == "" {
		return nil
	}
	if m.TrafficMode != trafficModeAuto || m.LatestRevision == m.LastReady {
		// No cut pending — but retire still runs: manually pinned traffic
		// must not exempt a deployment from revision GC (repeated Applies
		// would otherwise accumulate revisions without bound; retire already
		// protects lastReady and every weighted revision).
		return o.retire(ctx, m)
	}
	dep, err := o.client.AppsV1().Deployments(o.namespace).Get(ctx, objectNameFor(m.LatestRevision), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil // revision objects not materialized (yet)
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getRevision", err)
	}
	if !revisionAvailable(dep) {
		return nil // not ready (or failed) — never auto-cut
	}

	if err := o.writeRouteTraffic(ctx, m, []deployment.Target{{RevisionName: m.LatestRevision, Percent: 100}}); err != nil {
		return err
	}
	if err := o.updateMarker(ctx, m.ID, func(u *marker) { u.LastReady = m.LatestRevision }); err != nil {
		return err
	}
	m.LastReady = m.LatestRevision
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.RecordRolloutCut(ctx, time.Since(dep.CreationTimestamp.Time).Seconds())
	}
	slog.Info("Rollout auto-cut to latest revision", "deploymentId", m.ID, "revision", m.LatestRevision)
	return o.retire(ctx, m)
}

// retire garbage-collects revisions beyond the history limit (counted
// newest-first), never touching the last-ready revision or any revision still
// weighted in the current route.
func (o *Orchestrator) retire(ctx context.Context, m marker) error {
	revs, err := o.revisionNames(ctx, m.ID)
	if err != nil {
		return err
	}
	protected := map[string]bool{m.LastReady: true}
	for _, t := range o.currentTargets(ctx, m) {
		if t.Percent > 0 {
			protected[t.RevisionName] = true
		}
	}
	for i, rev := range revs {
		if i < o.cfg.RevisionHistoryLimit || protected[rev] {
			continue
		}
		if err := o.deleteRevisionObjects(ctx, rev); err != nil {
			return err
		}
		slog.Info("Retired revision", "deploymentId", m.ID, "revision", rev)
	}
	return nil
}

// revisionAvailable reports whether a revision's Deployment has at least one
// available replica — the auto-cut readiness gate.
func revisionAvailable(dep *appsv1.Deployment) bool {
	return dep.Status.AvailableReplicas >= 1
}
