// Package kubernetes implements the deployment.Orchestrator interface using
// the Kubernetes API. A deployment is a series of immutable Revision CRs;
// their pods are created directly by this service and exposed by selectorless
// Services fronted by a Gateway
// API HTTPRoute holding the traffic table, anchored by a marker ConfigMap.
// Kubernetes is the source of truth — Status, List, Spec, and Endpoints
// derive from it live, so any replica can serve any request and a restart
// loses nothing.
package kubernetes

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/deployment"
	"orchestrator/internal/deployment/endpointflip"
	"orchestrator/internal/kube"
	"orchestrator/internal/pool"
	revisionapi "orchestrator/internal/revision"
	"orchestrator/internal/volume"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"
	"slices"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// Orchestrator implements deployment.Orchestrator using Kubernetes.
type Orchestrator struct {
	client     kubernetes.Interface
	revisions  *revisionapi.Client
	gateway    gatewayclient.Interface
	namespace  string
	cfg        Config
	stop       context.CancelFunc
	controller *revisionController
	pools      *warm.Manager

	leaderMu      sync.Mutex
	leaderTerm    context.Context
	leaderChanged chan struct{}
	started       bool
}

// NewOrchestrator creates a Kubernetes deployment orchestrator.
func NewOrchestrator(ctx context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	if err := validateDeploymentPools(cfg.Pools); err != nil {
		return nil, err
	}
	// One rest config, so the configured budget is a single bucket shared by
	// the typed, dynamic, and Gateway clients rather than one bucket each.
	restCfg, err := kube.NewConfig(cfg.Kubeconfig, cfg.Context, cfg.Metrics, float32(cfg.ClientQPS), cfg.ClientBurst)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	gw, err := gatewayclient.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic kube client: %w", err)
	}
	o := &Orchestrator{client: cs, revisions: revisionapi.NewClient(dynamicClient), gateway: gw, namespace: cfg.Namespace, cfg: cfg}
	if len(cfg.Pools) > 0 {
		o.pools, err = NewRevisionPoolManager(cs, cfg)
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

// NewRevisionPoolManager builds the request-path half of Revision pooling.
// The deployments service uses it to claim and bind warm pods; the standalone
// revision-pool-controller uses the same contract to maintain inventory.
func NewRevisionPoolManager(client kubernetes.Interface, cfg Config) (*warm.Manager, error) {
	cfg.applyDefaults()
	if err := validateDeploymentPools(cfg.Pools); err != nil {
		return nil, err
	}
	return warm.New(client, cfg.Pools, warm.Config{
		Namespace: cfg.Namespace, SidecarImage: cfg.SidecarImage, ShimImage: cfg.PoolShimImage,
		SidecarImagePullPolicy: cfg.SidecarImagePullPolicy, WorkerImagePullPolicy: cfg.WorkerImagePullPolicy,
		RunAsUser: cfg.RunAsUser, Overcommit: cfg.Overcommit, Tolerations: cfg.Tolerations,
		NodeSelector: cfg.NodeSelector, RuntimeClasses: cfg.RuntimeClasses, Metrics: cfg.Metrics,
		LeaderElection: cfg.LeaderElection,
		Naming: warm.Naming{ManagedBy: ManagedByValue, Kind: "revision", Pool: "pool.id",
			Claim: LabelPoolClaim, Spec: "deployment.pool-claim-spec", NamePrefix: "pool", SecretName: "pool-claim-key"},
	}), nil
}

func validateDeploymentPools(pools []pool.Pool) error {
	for i := range pools {
		p := &pools[i]
		if p.CPU <= 0 || p.Memory <= 0 {
			return fmt.Errorf("deployment pool %q: cpu and memory are required for exact shape matching", p.ID)
		}
		if p.Command != "" || len(p.Environment) != 0 {
			return fmt.Errorf("deployment pool %q: command and environment are request-time fields and must not be configured on the pool", p.ID)
		}
		for j := range i {
			if poolShapeKey(&pools[j].Spec) == poolShapeKey(&p.Spec) {
				return fmt.Errorf("deployment pools %q and %q declare the same fixed shape", pools[j].ID, p.ID)
			}
		}
	}
	return nil
}

// Start surveys pre-existing managed deployments (their markers), then
// launches the background reconcilers — the rollout loop (auto-cut + retire)
// and, when the gateway is enabled, the cold endpoint flip — under the
// leader-election config: with election enabled only the lease holder runs
// them; disabled, they run directly (single-replica mode).
func (o *Orchestrator) Start(ctx context.Context) error {
	if o.pools != nil {
		if err := o.pools.Verify(ctx); err != nil {
			return apperrors.Internal("kubernetes.verifyPools", err)
		}
	}
	if _, err := o.revisions.List(ctx, o.namespace, metav1.ListOptions{Limit: 1}); err != nil {
		return apperrors.Internal("kubernetes.listRevisions", err)
	}
	markers, err := o.client.CoreV1().ConfigMaps(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return apperrors.Internal("kubernetes.listMarkers", err)
	}
	slog.Info("Deployment orchestrator started", "namespace", o.namespace, "deployments", len(markers.Items))

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o.stop = cancel
	o.leaderMu.Lock()
	o.started = true
	if o.leaderChanged == nil {
		o.leaderChanged = make(chan struct{})
	}
	o.leaderMu.Unlock()
	o.controller = newRevisionController(o)
	if err := o.controller.start(runCtx); err != nil {
		cancel()
		o.leaderMu.Lock()
		o.started = false
		o.leaderMu.Unlock()
		return apperrors.Internal("kubernetes.syncRevisionCaches", err)
	}
	go kube.RunLeaderElected(runCtx, o.client, o.namespace, o.cfg.LeaderElection, o.runReconcilers, o.onLeadership)
	return nil
}

// onLeadership records leadership transitions when metrics are wired.
func (o *Orchestrator) onLeadership(ctx context.Context, identity string, leading bool) {
	o.leaderMu.Lock()
	if leading {
		o.leaderTerm = ctx
	} else {
		o.leaderTerm = nil
	}
	if o.leaderChanged == nil {
		o.leaderChanged = make(chan struct{})
	} else {
		close(o.leaderChanged)
		o.leaderChanged = make(chan struct{})
	}
	o.leaderMu.Unlock()
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.RecordLeadership(ctx, identity, leading)
	}
}

// runReconcilers runs the background loops for one leadership term (or the
// process lifetime when election is disabled), blocking until ctx ends.
func (o *Orchestrator) runReconcilers(ctx context.Context) {
	go o.controller.runLeader(ctx)
	if o.cfg.GatewayEnabled {
		flip := endpointflip.New(o.client, o.namespace, endpointflip.Options{
			ActivatorSelector:  o.cfg.ActivatorSelector,
			ActivatorNamespace: o.cfg.ActivatorNamespace,
			ProxyPort:          workload.DefaultProxyPort,
			ActivatorPort:      int32(o.cfg.ActivatorPort),
		})
		go flip.Run(ctx)
	}
	o.runRollouts(ctx)
}

// RunLeaderElected runs `run` during this orchestrator's existing leadership
// term. It deliberately does not start another elector: two electors in one
// process with the same identity race their own Lease resource versions.
// Callers may subscribe after leadership was acquired and start immediately.
// Before Start, the disabled-election case retains its direct-run behavior for
// simple single-replica embedding.
func (o *Orchestrator) RunLeaderElected(ctx context.Context, run func(context.Context)) {
	o.leaderMu.Lock()
	started := o.started
	o.leaderMu.Unlock()
	if !started && !o.cfg.LeaderElection.Enabled {
		run(ctx)
		return
	}

	for ctx.Err() == nil {
		o.leaderMu.Lock()
		term := o.leaderTerm
		changed := o.leaderChanged
		if changed == nil {
			changed = make(chan struct{})
			o.leaderChanged = changed
		}
		o.leaderMu.Unlock()
		if term == nil {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}

		runWithinLeadershipTerm(ctx, term, run)
		// A leader-gated component is expected to block for the term. If it
		// returns early, wait for a real leadership transition rather than
		// immediately starting a duplicate copy in the same term.
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

func runWithinLeadershipTerm(ctx, term context.Context, run func(context.Context)) {
	runCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(term, cancel)
	run(runCtx)
	stop()
	cancel()
}

// Apply is the declarative create-or-update:
//   - no marker → the deployment is new: mint revision {id}-00001 and route
//     100% of traffic to it (the cold endpoint flip covers it until ready);
//   - identical spec → heal any missing revision objects, otherwise no-op;
//   - changed spec → mint the next revision. Traffic is UNTOUCHED — the
//     rollout reconciler auto-cuts once the new revision reports ready
//     (unless traffic was pinned manually).
func (o *Orchestrator) Apply(ctx context.Context, req *deployment.Request) (bool, error) {
	if err := o.checkRuntimeClass(ctx, req.RuntimeClass); err != nil {
		return false, err
	}
	specJSON, err := json.Marshal(req)
	if err != nil {
		return false, apperrors.Internal("kubernetes.marshalSpec", err)
	}

	m, err := o.getMarker(ctx, req.ID)
	switch {
	case errors.Is(err, apperrors.ErrNotFound):
		return true, o.createFirstRevision(ctx, req, string(specJSON))
	case err != nil:
		return false, err
	}
	stored, err := o.getSpecJSON(ctx, req.ID)
	if err != nil && !errors.Is(err, errSpecMissing) {
		return false, err
	}
	if err == nil && stored == string(specJSON) {
		// Identical spec — ensure the head revision's objects still exist.
		if err := o.ensureRevisionObjects(ctx, req, m.LatestRevision); err != nil {
			return false, err
		}
		return false, o.ensureRoute(ctx, m, fallbackTargets(m))
	}
	// Changed spec — or a marker whose spec Secret is gone, healed by minting
	// a fresh head so the stored spec always describes latestRevision.
	return false, o.mintNextRevision(ctx, req, m, string(specJSON))
}

func requestMatchesPool(req *deployment.Request, p *pool.Pool) bool {
	key := requestAcquisitionKey(req)
	return key != "" && poolShapeKey(&p.Spec) == key
}

func requestAcquisitionKey(req *deployment.Request) string {
	// The shim replaces the image entrypoint, so a claim must carry the
	// command explicitly. A custom workspace and kubelet-run probes are also
	// impossible to late-bind after a warm pod has started.
	if req.Command == "" || workspaceOf(req) != workspacePath ||
		(req.Probes != nil && (req.Probes.Liveness != nil || req.Probes.Startup != nil)) {
		return ""
	}
	return poolShapeKey(&pool.Spec{
		Image: req.Image, Port: req.Port, CPU: req.CPU, Memory: req.Memory,
		RuntimeClass: req.RuntimeClass, Volumes: req.Volumes, Mounts: artifact.HasMount(req.Artifacts),
		TerminationGracePeriodSeconds: req.TerminationGracePeriodSeconds,
	})
}

func poolShapeKey(shape *pool.Spec) string {
	canonical := struct {
		Image                         string          `json:"image"`
		Port                          int             `json:"port"`
		CPU                           float64         `json:"cpu"`
		Memory                        int             `json:"memory"`
		RuntimeClass                  string          `json:"runtimeClass"`
		Volumes                       []volume.Volume `json:"volumes,omitempty"`
		Mounts                        bool            `json:"mounts,omitempty"`
		TerminationGracePeriodSeconds int             `json:"terminationGracePeriodSeconds"`
	}{
		Image: shape.Image, Port: shape.Port, CPU: shape.CPU, Memory: shape.Memory,
		RuntimeClass: runtimeTier(shape.RuntimeClass), Volumes: canonicalVolumes(shape.Volumes), Mounts: shape.Mounts,
		TerminationGracePeriodSeconds: gracePeriodSeconds(shape.TerminationGracePeriodSeconds),
	}
	encoded, _ := json.Marshal(canonical)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func canonicalVolumes(volumes []volume.Volume) []volume.Volume {
	canonical := append([]volume.Volume(nil), volumes...)
	slices.SortFunc(canonical, func(a, b volume.Volume) int {
		if n := cmp.Compare(a.Source, b.Source); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Path, b.Path); n != 0 {
			return n
		}
		if n := cmp.Compare(a.SubPath, b.SubPath); n != 0 {
			return n
		}
		if a.ReadOnly == b.ReadOnly {
			return 0
		}
		if !a.ReadOnly {
			return -1
		}
		return 1
	})
	return canonical
}

func runtimeTier(tier string) string {
	if tier == "" {
		return deployment.RuntimeClassRunc
	}
	return tier
}

func gracePeriodSeconds(seconds int) int {
	if seconds == 0 {
		return 30
	}
	return seconds
}

func (o *Orchestrator) poolForRevision(revision *revisionapi.Revision) *pool.Pool {
	if revision.Spec.AcquisitionKey != "" {
		for i := range o.cfg.Pools {
			if poolShapeKey(&o.cfg.Pools[i].Spec) == revision.Spec.AcquisitionKey {
				return &o.cfg.Pools[i]
			}
		}
		return nil
	}
	if revision.Spec.Pool != "" && o.pools != nil {
		return o.pools.Pool(revision.Spec.Pool)
	}
	return nil
}

// checkRuntimeClass verifies the tier's RuntimeClass is installed
// before any revision is minted — a missing class would otherwise strand the
// revision's pods Pending.
func (o *Orchestrator) checkRuntimeClass(ctx context.Context, tier string) error {
	rc := kube.RuntimeClassFor(o.cfg.RuntimeClasses, tier)
	if rc == "" {
		return nil
	}
	_, err := o.client.NodeV1().RuntimeClasses().Get(ctx, rc, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return apperrors.Validation("runtimeClass", fmt.Sprintf("RuntimeClass %q (tier %q) is not installed", rc, tier))
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getRuntimeClass", err)
	}
	return nil
}

// createFirstRevision brings a brand-new deployment up: marker, revision
// {id}-00001, and its HTTPRoute at 100%. lastReady stays empty — the rollout
// reconciler records it once the revision reports ready.
func (o *Orchestrator) createFirstRevision(ctx context.Context, req *deployment.Request, specJSON string) error {
	rev := revisionName(req.ID, 1)
	m := marker{
		ID:             req.ID,
		Hosts:          req.Hosts,
		LatestRevision: rev,
		TrafficMode:    trafficModeAuto,
	}
	if err := o.createMarker(ctx, m); err != nil {
		return err
	}
	if err := o.writeSpecJSON(ctx, req.ID, specJSON); err != nil {
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
	if err := o.writeSpecJSON(ctx, req.ID, specJSON); err != nil {
		return err
	}
	if err := o.updateMarker(ctx, req.ID, func(m *marker) {
		m.LatestRevision = rev
		m.Hosts = req.Hosts
	}); err != nil {
		return err
	}
	m.Hosts = req.Hosts
	return o.ensureRoute(ctx, m, fallbackTargets(m))
}

// ensureRevisionObjects creates the revision's Revision CR, PDB (durably
// multi-replica deployments only), and Service, tolerating pre-existing ones —
// revisions are immutable, so create-if-missing is also the heal for a
// partial earlier Apply.
func (o *Orchestrator) ensureRevisionObjects(ctx context.Context, req *deployment.Request, rev string) error {
	candidate := buildRevision(req, o.cfg, rev)
	candidate.Spec.AcquisitionKey = requestAcquisitionKey(req)
	revision, err := o.revisions.Create(ctx, o.namespace, candidate)
	if apierrors.IsAlreadyExists(err) {
		revision, err = o.revisions.Get(ctx, o.namespace, objectNameFor(rev))
	}
	if err != nil {
		return apperrors.Internal("kubernetes.createRevision", err)
	}
	if pdb := buildPDB(req, rev); pdb != nil {
		// The PDB is deleted explicitly with the revision; the ownerReference
		// is belt-and-braces so GC reaps it if the Revision goes first.
		pdb.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: revisionapi.APIVersion(), Kind: revisionapi.Kind, Name: revision.Name, UID: revision.UID,
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
	return o.reconcileBeforeStart(ctx, objectNameFor(rev))
}

// reconcileBeforeStart preserves the convenient synchronous behavior used by
// small embeddings and unit tests. Once Start has installed the informer and
// leader-gated workers, they are the sole pod writers. Letting every API
// replica reconcile synchronously as well is harmless for deterministic
// direct-pod names but can double-claim a warm slot across processes.
func (o *Orchestrator) reconcileBeforeStart(ctx context.Context, revision string) error {
	o.leaderMu.Lock()
	started := o.started
	o.leaderMu.Unlock()
	if started {
		return nil
	}
	return o.reconcileRevision(ctx, revision)
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

	revision, err := o.revisions.Get(ctx, o.namespace, objectNameFor(rev))
	if apierrors.IsNotFound(err) {
		return apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getScale", err)
	}
	if revision.Spec.Replicas == int32(replicas) {
		return nil
	}
	if err := o.revisions.Scale(ctx, o.namespace, objectNameFor(rev), int32(replicas)); err != nil {
		return apperrors.Internal("kubernetes.updateScale", err)
	}
	return o.reconcileBeforeStart(ctx, objectNameFor(rev))
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

// Delete tears down the deployment: its HTTPRoute, every Revision and its
// Pods and Service, and finally the marker — last, so a crashed teardown is
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
	if err := o.deleteSpecSecret(ctx, id); err != nil {
		return err
	}
	err = o.client.CoreV1().ConfigMaps(o.namespace).Delete(ctx, objectNameFor(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteMarker", err)
	}
	return nil
}

// deleteRevisionObjects removes one revision's Revision, Pods, PDB, and Service,
// tolerating already-gone objects (not every revision has a PDB).
func (o *Orchestrator) deleteRevisionObjects(ctx context.Context, rev string) error {
	prop := metav1.DeletePropagationForeground
	err := o.revisions.Delete(ctx, o.namespace, objectNameFor(rev), metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteRevision", err)
	}
	err = o.client.CoreV1().Pods(o.namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: LabelRevision + "=" + rev,
	})
	if err != nil {
		return apperrors.Internal("kubernetes.deleteRevisionPods", err)
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

// Spec reconstructs the head revision's request from the dep-{id} spec
// Secret. No marker is NotFound; a marker whose Secret is gone is a clear
// Internal error (the next Apply heals it).
func (o *Orchestrator) Spec(ctx context.Context, id string) (*deployment.Request, error) {
	if _, err := o.getMarker(ctx, id); err != nil {
		return nil, err
	}
	specJSON, err := o.getSpecJSON(ctx, id)
	if errors.Is(err, errSpecMissing) {
		return nil, apperrors.Internal("kubernetes.getSpecSecret",
			fmt.Errorf("deployment %s exists but its spec Secret %s is missing; re-Apply to heal", id, objectNameFor(id)))
	}
	if err != nil {
		return nil, err
	}
	var req deployment.Request
	if err := json.Unmarshal([]byte(specJSON), &req); err != nil {
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
