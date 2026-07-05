// Package kubernetes implements the deployment.Orchestrator interface using
// the Kubernetes API. A deployment is a series of immutable revisions — each
// an apps/v1.Deployment plus a selectorless Service — fronted by a Gateway
// API HTTPRoute holding the traffic table, anchored by a marker ConfigMap.
// Kubernetes is the source of truth — Status, List, Spec, and Endpoints
// derive from it live, so any replica can serve any request and a restart
// loses nothing.
package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/deployment/endpointflip"
	"orchestrator/internal/kube"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// Orchestrator implements deployment.Orchestrator using Kubernetes.
type Orchestrator struct {
	client    kubernetes.Interface
	gateway   gatewayclient.Interface
	namespace string
	cfg       Config
	stop      context.CancelFunc
}

// NewOrchestrator creates a Kubernetes deployment orchestrator.
func NewOrchestrator(ctx context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	cs, err := kube.NewClient(cfg.Kubeconfig, cfg.Context, nil)
	if err != nil {
		return nil, err
	}
	gw, err := kube.NewGatewayClient(cfg.Kubeconfig, cfg.Context)
	if err != nil {
		return nil, err
	}
	return &Orchestrator{client: cs, gateway: gw, namespace: cfg.Namespace, cfg: cfg}, nil
}

// Start surveys pre-existing managed deployments (their markers), then
// launches the background reconcilers — the rollout loop (auto-cut + retire)
// and, when the gateway is enabled, the cold endpoint flip — under the
// leader-election config: with election enabled only the lease holder runs
// them; disabled, they run directly (single-replica mode).
func (o *Orchestrator) Start(ctx context.Context) error {
	markers, err := o.client.CoreV1().ConfigMaps(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return apperrors.Internal("kubernetes.listMarkers", err)
	}
	slog.Info("Deployment orchestrator started", "namespace", o.namespace, "deployments", len(markers.Items))

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.stop = cancel
	go kube.RunLeaderElected(runCtx, o.client, o.namespace, o.cfg.LeaderElection, o.runReconcilers, nil)
	return nil
}

// runReconcilers runs the background loops for one leadership term (or the
// process lifetime when election is disabled), blocking until ctx ends.
func (o *Orchestrator) runReconcilers(ctx context.Context) {
	if o.cfg.GatewayEnabled {
		flip := endpointflip.New(o.client, o.namespace, endpointflip.Options{
			ActivatorSelector: o.cfg.ActivatorSelector,
			ProxyPort:         proxy.DefaultProxyPort,
			ActivatorPort:     int32(o.cfg.ActivatorPort),
		})
		go flip.Run(ctx)
	}
	o.runRollouts(ctx)
}

// RunLeaderElected runs `run` under the orchestrator's leader-election config
// — the SAME lease that gates the built-in reconcilers, so one elected replica
// runs every leader-gated loop (rollouts, endpoint flip, and the caller's,
// e.g. the shared autoscaler). With election disabled it simply calls
// run(ctx). Blocks until ctx cancels. Deliberately not part of
// deployment.Orchestrator: it is Kubernetes-specific wiring for main.
func (o *Orchestrator) RunLeaderElected(ctx context.Context, run func(context.Context)) {
	kube.RunLeaderElected(ctx, o.client, o.namespace, o.cfg.LeaderElection, run, nil)
}

// Apply is the declarative create-or-update:
//   - no marker → the deployment is new: mint revision {id}-00001 and route
//     100% of traffic to it (the cold endpoint flip covers it until ready);
//   - identical spec → heal any missing revision objects, otherwise no-op;
//   - changed spec → mint the next revision. Traffic is UNTOUCHED — the
//     rollout reconciler auto-cuts once the new revision reports ready
//     (unless traffic was pinned manually).
func (o *Orchestrator) Apply(ctx context.Context, req *deployment.Request) error {
	specJSON, err := json.Marshal(req)
	if err != nil {
		return apperrors.Internal("kubernetes.marshalSpec", err)
	}

	m, err := o.getMarker(ctx, req.ID)
	switch {
	case errors.Is(err, apperrors.ErrNotFound):
		return o.createFirstRevision(ctx, req, string(specJSON))
	case err != nil:
		return err
	case m.Spec == string(specJSON):
		// Identical spec — ensure the head revision's objects still exist.
		if err := o.ensureRevisionObjects(ctx, req, m.LatestRevision); err != nil {
			return err
		}
		return o.ensureRoute(ctx, m, fallbackTargets(m))
	default:
		return o.mintNextRevision(ctx, req, m, string(specJSON))
	}
}

// createFirstRevision brings a brand-new deployment up: marker, revision
// {id}-00001, and its HTTPRoute at 100%. lastReady stays empty — the rollout
// reconciler records it once the revision reports ready.
func (o *Orchestrator) createFirstRevision(ctx context.Context, req *deployment.Request, specJSON string) error {
	rev := revisionName(req.ID, 1)
	m := marker{
		ID:             req.ID,
		Host:           req.Host,
		Spec:           specJSON,
		LatestRevision: rev,
		TrafficMode:    trafficModeAuto,
	}
	if err := o.createMarker(ctx, m); err != nil {
		return err
	}
	if err := o.ensureRevisionObjects(ctx, req, rev); err != nil {
		return err
	}
	return o.ensureRoute(ctx, m, []deployment.Target{{RevisionName: rev, Percent: 100}})
}

// mintNextRevision materializes the changed spec as a new immutable revision
// and records it as the head. Traffic is left alone.
func (o *Orchestrator) mintNextRevision(ctx context.Context, req *deployment.Request, m marker, specJSON string) error {
	rev := revisionName(req.ID, revisionNumber(m.LatestRevision)+1)
	if err := o.ensureRevisionObjects(ctx, req, rev); err != nil {
		return err
	}
	if err := o.updateMarker(ctx, req.ID, func(m *marker) {
		m.Spec = specJSON
		m.LatestRevision = rev
		m.Host = req.Host
	}); err != nil {
		return err
	}
	m.Host = req.Host
	return o.ensureRoute(ctx, m, fallbackTargets(m))
}

// ensureRevisionObjects creates the revision's Deployment, PDB (durably
// multi-replica deployments only), and Service, tolerating pre-existing ones —
// revisions are immutable, so create-if-missing is also the heal for a
// partial earlier Apply.
func (o *Orchestrator) ensureRevisionObjects(ctx context.Context, req *deployment.Request, rev string) error {
	dep, err := o.client.AppsV1().Deployments(o.namespace).Create(ctx, buildDeployment(req, o.cfg, rev), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		dep, err = o.client.AppsV1().Deployments(o.namespace).Get(ctx, objectNameFor(rev), metav1.GetOptions{})
	}
	if err != nil {
		return apperrors.Internal("kubernetes.createDeployment", err)
	}
	if pdb := buildPDB(req, rev); pdb != nil {
		// The PDB is deleted explicitly with the revision; the ownerReference
		// is belt-and-braces so GC reaps it if the Deployment goes first.
		pdb.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID,
		}}
		_, err = o.client.PolicyV1().PodDisruptionBudgets(o.namespace).Create(ctx, pdb, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return apperrors.Internal("kubernetes.createPodDisruptionBudget", err)
		}
	}
	_, err = o.client.CoreV1().Services(o.namespace).Create(ctx, buildService(req.ID, rev), metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return apperrors.Internal("kubernetes.createService", err)
	}
	return nil
}

// Scale sets the replica count of the ROUTED revision via the scale
// subresource — the same write the activator's cold raise and the
// idle-to-zero loop perform, so they can't conflict with a concurrent Apply
// (which never touches a live revision's spec.replicas).
func (o *Orchestrator) Scale(ctx context.Context, id string, replicas int) error {
	if replicas < 0 {
		replicas = 0
	}
	m, err := o.getMarker(ctx, id)
	if err != nil {
		return err
	}
	rev := o.routedRevision(ctx, m)

	deployments := o.client.AppsV1().Deployments(o.namespace)
	scale, err := deployments.GetScale(ctx, objectNameFor(rev), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getScale", err)
	}
	if scale.Spec.Replicas == int32(replicas) {
		return nil
	}
	scale.Spec.Replicas = int32(replicas)
	if _, err := deployments.UpdateScale(ctx, objectNameFor(rev), scale, metav1.UpdateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.updateScale", err)
	}
	return nil
}

// routedRevision picks the revision a whole-deployment operation acts on: a
// single 100% target names it outright; under a split it is the latest-ready
// revision (falling back to the head).
func (o *Orchestrator) routedRevision(ctx context.Context, m marker) string {
	targets := o.currentTargets(ctx, m)
	if len(targets) == 1 {
		return targets[0].RevisionName
	}
	if m.LastReady != "" {
		return m.LastReady
	}
	return m.LatestRevision
}

// Delete tears down the deployment: its HTTPRoute, every revision's
// Deployment (foreground propagation, so pods are gone before the objects
// are) and Service, and finally the marker — last, so a crashed teardown is
// still visible and retryable.
func (o *Orchestrator) Delete(ctx context.Context, id string) error {
	if _, err := o.getMarker(ctx, id); err != nil {
		return err
	}
	if err := o.deleteRoute(ctx, id); err != nil {
		return err
	}
	revs, err := o.revisionNames(ctx, id)
	if err != nil {
		return err
	}
	for _, rev := range revs {
		if err := o.deleteRevisionObjects(ctx, rev); err != nil {
			return err
		}
	}
	err = o.client.CoreV1().ConfigMaps(o.namespace).Delete(ctx, objectNameFor(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteMarker", err)
	}
	return nil
}

// deleteRevisionObjects removes one revision's Deployment, PDB, and Service,
// tolerating already-gone objects (not every revision has a PDB).
func (o *Orchestrator) deleteRevisionObjects(ctx context.Context, rev string) error {
	prop := metav1.DeletePropagationForeground
	err := o.client.AppsV1().Deployments(o.namespace).Delete(ctx, objectNameFor(rev), metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteDeployment", err)
	}
	err = o.client.PolicyV1().PodDisruptionBudgets(o.namespace).Delete(ctx, objectNameFor(rev), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deletePodDisruptionBudget", err)
	}
	err = o.client.CoreV1().Services(o.namespace).Delete(ctx, objectNameFor(rev), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteService", err)
	}
	return nil
}

// Spec reconstructs the head revision's request from the marker.
func (o *Orchestrator) Spec(ctx context.Context, id string) (*deployment.Request, error) {
	m, err := o.getMarker(ctx, id)
	if err != nil {
		return nil, err
	}
	var req deployment.Request
	if err := json.Unmarshal([]byte(m.Spec), &req); err != nil {
		return nil, apperrors.Internal("kubernetes.unmarshalSpec", err)
	}
	return &req, nil
}

// Ready checks that the K8s API server is reachable.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Discovery().ServerVersion()
	return err
}

// Close stops the background reconcilers. Running deployments are NOT
// stopped — Kubernetes keeps serving them independently.
func (o *Orchestrator) Close() error {
	if o.stop != nil {
		o.stop()
	}
	return nil
}

// Verify Orchestrator implements deployment.Orchestrator.
var _ deployment.Orchestrator = (*Orchestrator)(nil)
