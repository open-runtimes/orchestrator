package kubernetes

import (
	"context"
	"errors"
	"orchestrator/internal/deployment"
	"orchestrator/internal/pool"
	revisionapi "orchestrator/internal/revision"
	"orchestrator/internal/volume"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
)

type revisionPoolSidecar struct{ claim *workload.ClaimRequest }

func (f *revisionPoolSidecar) Claim(_ context.Context, _ string, _ string, req *workload.ClaimRequest) error {
	copy := *req
	f.claim = &copy
	return nil
}
func (*revisionPoolSidecar) State(context.Context, string) (*workload.ClaimState, error) {
	return &workload.ClaimState{}, nil
}
func (*revisionPoolSidecar) Ready(context.Context, string) bool              { return true }
func (*revisionPoolSidecar) Requests(context.Context, string) (int64, error) { return 0, nil }

func TestRevisionClaimsWarmPoolPod(t *testing.T) {
	o, cs := newTestOrchestrator(t)
	p := pool.Pool{ID: "node", Size: 1, Burst: pool.BurstReject, Spec: pool.Spec{
		Image: "nginx:1.27", Port: 8080, CPU: 0.5, Memory: 128,
	}}
	o.cfg.Pools = []pool.Pool{p}
	sidecar := &revisionPoolSidecar{}
	o.pools = warm.New(cs, []pool.Pool{p}, warm.Config{
		Namespace: o.namespace, SidecarImage: "sidecar", ShimImage: "shim", RunAsUser: 65532,
		Naming: warm.Naming{ManagedBy: ManagedByValue, Kind: "revision", Pool: "pool.id",
			Claim: "deployment.pool-claim", Spec: "deployment.pool-claim-spec", NamePrefix: "pool", SecretName: "pool-claim-key"},
		Client: sidecar,
	})
	if err := o.pools.Verify(t.Context()); err != nil {
		t.Fatalf("verify pools: %v", err)
	}
	warmPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-node-abc", Namespace: o.namespace,
			Labels: map[string]string{warm.LabelManagedBy: ManagedByValue, "pool.id": "node"}},
		Status: corev1.PodStatus{PodIP: "10.0.0.8", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if _, err := cs.CoreV1().Pods(o.namespace).Create(t.Context(), warmPod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	req := testRequest()
	req.Replicas = 1
	if _, err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	revision, err := o.revisions.Get(t.Context(), o.namespace, objectNameFor("web-00001"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), warmPod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Labels[LabelRevision] != "web-00001" || claimed.Labels[LabelReplicaSlot] != "0" {
		t.Fatalf("revision labels = %#v", claimed.Labels)
	}
	if claimed.Labels[LabelServing] != "true" {
		t.Fatalf("claimed pod was exposed before serving: labels = %#v", claimed.Labels)
	}
	if len(claimed.OwnerReferences) != 1 || claimed.OwnerReferences[0].Name != revision.Name {
		t.Fatalf("owner references = %#v", claimed.OwnerReferences)
	}
	if sidecar.claim == nil || sidecar.claim.Port != 8080 || sidecar.claim.ClaimID == "" {
		t.Fatalf("claim request = %#v", sidecar.claim)
	}
}

func TestRevisionPoolExhaustionFallsBackToDirectPod(t *testing.T) {
	o, cs := newTestOrchestrator(t)
	p := pool.Pool{ID: "node", Size: 1, Burst: pool.BurstReject, Spec: pool.Spec{
		Image: "nginx:1.27", Port: 8080, CPU: 0.5, Memory: 128,
	}}
	o.cfg.Pools = []pool.Pool{p}
	o.pools = warm.New(cs, []pool.Pool{p}, warm.Config{
		Namespace: o.namespace, SidecarImage: "sidecar", ShimImage: "shim", RunAsUser: 65532,
		Naming: warm.Naming{ManagedBy: ManagedByValue, Kind: "revision", Pool: "pool.id",
			Claim: "deployment.pool-claim", Spec: "deployment.pool-claim-spec", NamePrefix: "pool", SecretName: "pool-claim-key"},
		Client: &revisionPoolSidecar{},
	})
	if err := o.pools.Verify(t.Context()); err != nil {
		t.Fatalf("verify pools: %v", err)
	}
	req := testRequest()
	req.Replicas = 1
	if _, err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	revision, err := o.revisions.Get(t.Context(), o.namespace, objectNameFor("web-00001"))
	if err != nil {
		t.Fatal(err)
	}
	if revision.Spec.AcquisitionKey == "" || revision.Spec.Pool != "" || revision.Spec.Template == nil {
		t.Fatalf("revision must retain acquisition key and direct template: %+v", revision.Spec)
	}
	pods, err := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Labels[LabelPoolClaim] != "" || pods.Items[0].Name != "dep-web-00001-0" {
		t.Fatalf("direct fallback pods = %#v", pods.Items)
	}
}

func TestExistingRevisionUsesMatchingPoolAddedLater(t *testing.T) {
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 1
	if _, err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("initial direct apply: %v", err)
	}

	p := pool.Pool{ID: "node", Size: 1, Burst: pool.BurstReject, Spec: pool.Spec{
		Image: req.Image, Port: req.Port, CPU: req.CPU, Memory: req.Memory,
	}}
	o.cfg.Pools = []pool.Pool{p}
	sidecar := &revisionPoolSidecar{}
	o.pools = warm.New(cs, []pool.Pool{p}, warm.Config{
		Namespace: o.namespace, SidecarImage: "sidecar", ShimImage: "shim", RunAsUser: 65532,
		Naming: warm.Naming{ManagedBy: ManagedByValue, Kind: "revision", Pool: "pool.id",
			Claim: "deployment.pool-claim", Spec: "deployment.pool-claim-spec", NamePrefix: "pool", SecretName: "pool-claim-key"},
		Client: sidecar,
	})
	if err := o.pools.Verify(t.Context()); err != nil {
		t.Fatalf("verify pools: %v", err)
	}
	warmPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-node-later", Namespace: o.namespace,
			Labels: map[string]string{warm.LabelManagedBy: ManagedByValue, "pool.id": "node"}},
		Status: corev1.PodStatus{PodIP: "10.0.0.9", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	if _, err := cs.CoreV1().Pods(o.namespace).Create(t.Context(), warmPod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := o.Scale(t.Context(), req.ID, 2); err != nil {
		t.Fatalf("scale after adding pool: %v", err)
	}
	claimed, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), warmPod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Labels[LabelRevision] != "web-00001" || claimed.Labels[LabelReplicaSlot] != "1" {
		t.Fatalf("later pool claim labels = %#v", claimed.Labels)
	}
}

func TestRequestMatchesPoolRequiresExactFixedShape(t *testing.T) {
	base := testRequest()
	p := pool.Pool{ID: "node", Spec: pool.Spec{
		Image: base.Image, Port: base.Port, CPU: base.CPU, Memory: base.Memory,
	}}
	if !requestMatchesPool(base, &p) {
		t.Fatal("equal shape did not match")
	}
	base.Volumes = []volume.Volume{{Source: "b", Path: "/b"}, {Source: "a", Path: "/a"}}
	p.Volumes = []volume.Volume{{Source: "a", Path: "/a"}, {Source: "b", Path: "/b"}}
	if !requestMatchesPool(base, &p) {
		t.Fatal("semantically equal volumes in a different order did not match")
	}
	for name, mutate := range map[string]func(*deployment.Request){
		"image":      func(r *deployment.Request) { r.Image = "nginx:other" },
		"port":       func(r *deployment.Request) { r.Port++ },
		"cpu":        func(r *deployment.Request) { r.CPU++ },
		"memory":     func(r *deployment.Request) { r.Memory++ },
		"workspace":  func(r *deployment.Request) { r.Workspace = "/srv" },
		"no command": func(r *deployment.Request) { r.Command = "" },
		"liveness": func(r *deployment.Request) {
			r.Probes = &deployment.Probes{Liveness: &deployment.Probe{Path: "/health"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *base
			mutate(&candidate)
			if requestMatchesPool(&candidate, &p) {
				t.Fatalf("mismatched %s was accepted", name)
			}
		})
	}
}

func TestValidateDeploymentPoolsRejectsNonMatchableAndDuplicateShapes(t *testing.T) {
	shape := pool.Spec{Image: "node:22", Port: 3000, CPU: 1, Memory: 512}
	if err := validateDeploymentPools([]pool.Pool{{ID: "node", Spec: shape}}); err != nil {
		t.Fatalf("valid pool: %v", err)
	}
	for name, pools := range map[string][]pool.Pool{
		"missing resources": {{ID: "node", Spec: pool.Spec{Image: "node:22", Port: 3000}}},
		"command default":   {{ID: "node", Spec: pool.Spec{Image: "node:22", Port: 3000, CPU: 1, Memory: 512, Command: "node app.js"}}},
		"environment":       {{ID: "node", Spec: pool.Spec{Image: "node:22", Port: 3000, CPU: 1, Memory: 512, Environment: map[string]string{"A": "1"}}}},
		"duplicate":         {{ID: "a", Spec: shape}, {ID: "b", Spec: shape}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDeploymentPools(pools); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDirectPods_ApplyCreatesDeterministicSlots(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())

	deployments, err := cs.AppsV1().Deployments(o.namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Deployments: %v", err)
	}
	if len(deployments.Items) != 0 {
		t.Fatalf("direct-pod backend created %d Kubernetes Deployments", len(deployments.Items))
	}
	pods, err := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("pods = %d, want 2", len(pods.Items))
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Labels[LabelReplicaSlot] == "" {
			t.Errorf("pod %s has no replica slot", pod.Name)
		}
		if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].Kind != "Revision" {
			t.Errorf("pod %s owner references = %+v", pod.Name, pod.OwnerReferences)
		}
	}
}

func TestRevisionLeaderAudit_UsesWarmCachesForTwoThousand(t *testing.T) {
	o, cs := newTestOrchestrator(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for i := range 2000 {
		req := testRequest()
		req.ID = "fleet"
		req.Replicas = 0
		revision := buildRevision(req, o.cfg, revisionName("fleet", i+1))
		// Model the production restart case: the baseline is settled before
		// leadership moves, so the audit should be a read-only cache walk.
		revision.Status = deriveRevisionStatus(revision, nil, nil)
		if _, err := o.revisions.Create(ctx, o.namespace, revision); err != nil {
			t.Fatalf("seed Revision %d: %v", i, err)
		}
	}
	controller := newRevisionController(o)
	if err := controller.start(ctx); err != nil {
		t.Fatalf("start caches: %v", err)
	}
	cs.ClearActions()
	started := time.Now()
	leaderCtx, stopLeader := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		controller.runLeader(leaderCtx)
		close(done)
	}()
	// The dynamic fake serializes 2,000 status-subresource writes. Keep this
	// threshold deliberately loose; real latency is covered by the kind
	// benchmark, while this test guards the cache-only API access pattern.
	deadline := time.Now().Add(10 * time.Second)
	seenAudit := false
	for time.Now().Before(deadline) {
		controller.mu.Lock()
		remaining := len(controller.initial)
		if controller.initial != nil {
			seenAudit = true
		}
		controller.mu.Unlock()
		if seenAudit && remaining == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	controller.mu.Lock()
	remaining := len(controller.initial)
	controller.mu.Unlock()
	stopLeader()
	<-done
	if remaining != 0 {
		t.Fatalf("leader audit left %d Revisions after %s", remaining, time.Since(started))
	}
	for _, action := range cs.Actions() {
		if action.GetResource().Resource == "pods" && (action.GetVerb() == "get" || action.GetVerb() == "list") {
			t.Fatalf("cached leader audit issued a Pod %s", action.GetVerb())
		}
	}
}

func TestRevisionReplicaDrift_IsLiveAggregateOnLeaderOnly(t *testing.T) {
	o, _ := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 2
	if _, err := o.revisions.Create(t.Context(), o.namespace, buildRevision(req, o.cfg, "web-00001")); err != nil {
		t.Fatalf("seed Revision: %v", err)
	}
	controller := newRevisionController(o)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := controller.start(ctx); err != nil {
		t.Fatalf("start caches: %v", err)
	}
	if got := controller.replicaDrift(); got != 0 {
		t.Fatalf("follower drift = %d, want 0", got)
	}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()
	controller.mu.Lock()
	controller.queue = queue
	controller.mu.Unlock()
	if got := controller.replicaDrift(); got != 2 {
		t.Fatalf("leader aggregate drift = %d, want 2", got)
	}
}

func TestDirectPods_TransientCreateFailureHeals(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 1
	revision := buildRevision(req, o.cfg, "web-00001")
	if _, err := o.revisions.Create(t.Context(), o.namespace, revision); err != nil {
		t.Fatalf("create Revision: %v", err)
	}
	var failed atomic.Bool
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failed.CompareAndSwap(false, true) {
			return true, nil, errors.New("transient API failure")
		}
		return false, nil, nil
	})
	// The direct path reports the rejection through the Ready condition, as
	// a ReplicaSet would, rather than failing Apply.
	if err := o.reconcileRevision(t.Context(), revision.Name); err != nil {
		t.Fatalf("direct reconcile surfaced the create failure: %v", err)
	}
	if got := readyCondition(t, o, revision.Name); got.Status != metav1.ConditionFalse || got.Reason != "ReplicaFailure" || got.Message != "transient API failure" {
		t.Fatalf("condition after rejected create = %+v", got)
	}
	if err := o.reconcileRevision(t.Context(), revision.Name); err != nil {
		t.Fatalf("retry did not heal: %v", err)
	}
	if _, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("healed Pod missing: %v", err)
	}
	if got := readyCondition(t, o, revision.Name); got.Status != metav1.ConditionUnknown {
		t.Fatalf("condition after heal = %+v, want PodsNotReady", got)
	}
}

func TestDirectPods_WorkerReturnsCreateFailureForRetry(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 1
	revision, err := o.revisions.Create(t.Context(), o.namespace, buildRevision(req, o.cfg, "web-00001"))
	if err != nil {
		t.Fatalf("create Revision: %v", err)
	}
	cs.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("exceeded quota")
	})
	if err := o.reconcileRevisionPods(t.Context(), revision, nil, false, time.Now()); err == nil || err.Error() != "exceeded quota" {
		t.Fatalf("worker reconcile error = %v, want the create failure for a rate-limited retry", err)
	}
	if got := readyCondition(t, o, revision.Name); got.Reason != "ReplicaFailure" {
		t.Fatalf("condition = %+v", got)
	}
}

func readyCondition(t *testing.T, o *Orchestrator, name string) metav1.Condition {
	t.Helper()
	revision, err := o.revisions.Get(t.Context(), o.namespace, name)
	if err != nil {
		t.Fatalf("get Revision: %v", err)
	}
	for _, condition := range revision.Status.Conditions {
		if condition.Type == revisionConditionReady {
			return condition
		}
	}
	t.Fatalf("Revision %s has no Ready condition: %+v", name, revision.Status)
	return metav1.Condition{}
}

func TestDirectPods_ReplacesTerminatingSlotImmediately(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 1
	revision, err := o.revisions.Create(t.Context(), o.namespace, buildRevision(req, o.cfg, "web-00001"))
	if err != nil {
		t.Fatalf("create Revision: %v", err)
	}
	// A pod stuck Terminating (graceful shutdown, or a node that went away)
	// still owns the deterministic slot name.
	stuck := buildRevisionPod(revision, 0, nil)
	now := metav1.Now()
	stuck.DeletionTimestamp = &now
	if _, err := cs.CoreV1().Pods(o.namespace).Create(t.Context(), stuck, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed terminating pod: %v", err)
	}
	for range 2 {
		// Idempotent: the second pass must not mint a third pod.
		if err := o.reconcileRevision(t.Context(), revision.Name); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	pods, err := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("pods = %d, want the terminating pod plus one replacement", len(pods.Items))
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Labels[LabelReplicaSlot] != "0" {
			t.Errorf("pod %s slot = %q, want 0", pod.Name, pod.Labels[LabelReplicaSlot])
		}
		if pod.DeletionTimestamp == nil && pod.Name == stuck.Name {
			t.Errorf("replacement reused the terminating pod's name %s", pod.Name)
		}
	}
	updated, err := o.revisions.Get(t.Context(), o.namespace, revision.Name)
	if err != nil {
		t.Fatalf("get Revision: %v", err)
	}
	if updated.Status.Replicas != 1 {
		t.Fatalf("status.replicas = %d, want 1 (terminating pods do not count)", updated.Status.Replicas)
	}
}

func TestDirectPods_StaleCacheLeavesNewerPodsAlone(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 0
	revision := buildRevision(req, o.cfg, "web-00001")
	revision.Generation = 3
	// A pod created by the synchronous Scale path for generation 4, observed
	// through a Revision cache still at generation 3 (replicas 0).
	pod := buildRevisionPod(revision, 0, nil)
	pod.Annotations[AnnotationRevisionGeneration] = "4"
	if _, err := cs.CoreV1().Pods(o.namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}
	if err := o.reconcileRevisionPods(t.Context(), revision, []*corev1.Pod{pod}, false, time.Now()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), pod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("stale reconcile deleted a pod from a newer generation: %v", err)
	}
}

func TestDirectPods_StatusConflictHeals(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 1
	revision := buildRevision(req, o.cfg, "web-00001")
	created, err := o.revisions.Create(t.Context(), o.namespace, revision)
	if err != nil {
		t.Fatalf("create Revision: %v", err)
	}
	client := o.revisions.Dynamic().(interface {
		PrependReactor(string, string, k8stesting.ReactionFunc)
	})
	var conflicted atomic.Bool
	client.PrependReactor("update", "revisions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" && conflicted.CompareAndSwap(false, true) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: revisionapi.Group, Resource: "revisions"}, created.Name, errors.New("concurrent status writer"))
		}
		return false, nil, nil
	})
	if err := o.reconcileRevision(t.Context(), created.Name); err != nil {
		t.Fatalf("status conflict was not retried: %v", err)
	}
	if !conflicted.Load() {
		t.Fatal("test did not inject a status conflict")
	}
}

func TestRevisionUpdate_StatusOnlyDoesNotRequeue(t *testing.T) {
	t.Parallel()
	old := &unstructured.Unstructured{}
	old.SetName("dep-web-00001")
	old.SetGeneration(1)
	current := old.DeepCopy()
	current.Object["status"] = map[string]any{"readyReplicas": int64(1)}

	if shouldEnqueueRevisionUpdate(old, current) {
		t.Fatal("status-only update requeued the Revision")
	}
	current.SetGeneration(2)
	if !shouldEnqueueRevisionUpdate(old, current) {
		t.Fatal("spec generation update did not requeue the Revision")
	}
}

func TestDirectPods_ScaleConvergesSlots(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())

	if err := o.Scale(t.Context(), "web", 1); err != nil {
		t.Fatalf("scale down: %v", err)
	}
	pods, _ := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if len(pods.Items) != 1 || pods.Items[0].Labels[LabelReplicaSlot] != "0" {
		t.Fatalf("scale down slots = %+v, want only slot 0", pods.Items)
	}

	if err := o.Scale(t.Context(), "web", 3); err != nil {
		t.Fatalf("scale up: %v", err)
	}
	pods, _ = cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if len(pods.Items) != 3 {
		t.Fatalf("scale up pods = %d, want 3", len(pods.Items))
	}
}

func TestDirectPods_ReplacesTerminalSlot(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())

	pod, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get slot: %v", err)
	}
	pod.Status.Phase = corev1.PodFailed
	if _, err := cs.CoreV1().Pods(o.namespace).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("fail slot: %v", err)
	}
	if err := o.reconcileRevision(t.Context(), "dep-web-00001"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	replacement, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get replacement: %v", err)
	}
	if replacement.Status.Phase == corev1.PodFailed {
		t.Fatal("terminal replica slot was not replaced")
	}
}

func TestDirectPods_RepairsDeletedAndDuplicateSlots(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 2
	mustApply(t, o, req)

	if err := cs.CoreV1().Pods(o.namespace).Delete(t.Context(), "dep-web-00001-0", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete slot 0: %v", err)
	}
	duplicate, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get slot 1: %v", err)
	}
	duplicate = duplicate.DeepCopy()
	duplicate.ResourceVersion = ""
	duplicate.Name = "duplicate-slot-1"
	if _, err := cs.CoreV1().Pods(o.namespace).Create(t.Context(), duplicate, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create duplicate: %v", err)
	}

	if err := o.reconcileRevision(t.Context(), "dep-web-00001"); err != nil {
		t.Fatalf("repair reconcile: %v", err)
	}
	pods, err := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if err != nil {
		t.Fatalf("list repaired Pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("repaired Pods = %d, want 2", len(pods.Items))
	}
	slots := map[string]int{}
	for i := range pods.Items {
		slots[pods.Items[i].Labels[LabelReplicaSlot]]++
	}
	if slots["0"] != 1 || slots["1"] != 1 {
		t.Fatalf("repaired slots = %v, want one each", slots)
	}
}

func TestDirectPods_DeletingRevisionDrainsWithoutRecreate(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	revision, err := o.revisions.Get(t.Context(), o.namespace, "dep-web-00001")
	if err != nil {
		t.Fatalf("get Revision: %v", err)
	}
	now := metav1.Now()
	revision.DeletionTimestamp = &now
	pod0, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get slot 0: %v", err)
	}
	pod1, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get slot 1: %v", err)
	}
	if err := o.reconcileRevisionPods(t.Context(), revision, []*corev1.Pod{pod0, pod1}, true, time.Now()); err != nil {
		t.Fatalf("deletion reconcile: %v", err)
	}
	pods, err := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelRevision + "=web-00001"})
	if err != nil {
		t.Fatalf("list Pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("deleting Revision retained/recreated %d Pods", len(pods.Items))
	}
}

func TestRevisionStatus_ProgressDeadline(t *testing.T) {
	t.Parallel()
	revision := buildRevision(testRequest(), Config{Namespace: "orchestrator"}, "web-00001")
	revision.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	revision.Spec.ReadyTimeoutSeconds = 1
	status := deriveRevisionStatus(revision, nil, nil)
	if len(status.Conditions) != 1 || status.Conditions[0].Status != metav1.ConditionFalse || status.Conditions[0].Reason != progressDeadlineExceeded {
		t.Fatalf("condition = %+v", status.Conditions)
	}
}

func TestRevisionStatus_DeadlineRunsFromLastUnready(t *testing.T) {
	t.Parallel()
	revision := buildRevision(testRequest(), Config{Namespace: "orchestrator"}, "web-00001")
	revision.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	revision.Generation = 2
	revision.Spec.Replicas = 1
	revision.Spec.ReadyTimeoutSeconds = 600
	// Scaled to zero an hour ago under the previous generation, now raised.
	revision.Status.Conditions = []metav1.Condition{{
		Type: revisionConditionReady, Status: metav1.ConditionTrue, Reason: "ScaledToZero",
		ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
	}}
	status := deriveRevisionStatus(revision, nil, nil)
	if got := status.Conditions[0]; got.Status != metav1.ConditionUnknown || time.Since(got.LastTransitionTime.Time) > time.Minute {
		t.Fatalf("cold start of an old Revision = %+v, want a fresh PodsNotReady clock", got)
	}
	revision.Status = status
	if delay := revisionDeadlineDelay(revision); delay < 9*time.Minute || delay > 10*time.Minute {
		t.Fatalf("deadline delay = %s, want ~10m from the transition", delay)
	}

	// Unready for longer than the timeout under the current generation.
	revision.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-11 * time.Minute))
	if delay := revisionDeadlineDelay(revision); delay != 0 {
		t.Fatalf("passed deadline delay = %s, want 0 (no hot loop)", delay)
	}
	status = deriveRevisionStatus(revision, nil, nil)
	if got := status.Conditions[0]; got.Status != metav1.ConditionFalse || got.Reason != progressDeadlineExceeded {
		t.Fatalf("condition = %+v", got)
	}
	// Exceeded stays exceeded until pods are ready; it does not re-arm.
	revision.Status = status
	if got := deriveRevisionStatus(revision, nil, nil).Conditions[0]; got.Reason != progressDeadlineExceeded {
		t.Fatalf("condition re-armed to %+v", got)
	}
	if delay := revisionDeadlineDelay(revision); delay != 0 {
		t.Fatalf("failed Revision scheduled a deadline pass in %s", delay)
	}
}

func TestRevisionStatus_ReportsImagePullFailure(t *testing.T) {
	t.Parallel()
	revision := buildRevision(testRequest(), Config{Namespace: "orchestrator"}, "web-00001")
	revision.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	revision.Spec.ReadyTimeoutSeconds = 1
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dep-web-00001-0", Labels: map[string]string{LabelReplicaSlot: "0"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "runtime",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "image not found",
			}},
		}}},
	}
	status := deriveRevisionStatus(revision, []corev1.Pod{pod}, nil)
	if got := status.Conditions[0].Message; got != "pod dep-web-00001-0: ImagePullBackOff: image not found" {
		t.Fatalf("failure message = %q", got)
	}
}
