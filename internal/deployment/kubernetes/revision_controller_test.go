package kubernetes

import (
	"context"
	"errors"
	revisionapi "orchestrator/internal/revision"
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
		revision.Status = deriveRevisionStatus(revision, nil)
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
	if err := o.reconcileRevision(t.Context(), revision.Name); err == nil {
		t.Fatal("first reconcile unexpectedly succeeded")
	}
	if err := o.reconcileRevision(t.Context(), revision.Name); err != nil {
		t.Fatalf("retry did not heal: %v", err)
	}
	if _, err := cs.CoreV1().Pods(o.namespace).Get(t.Context(), "dep-web-00001-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("healed Pod missing: %v", err)
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
	status := deriveRevisionStatus(revision, nil)
	if len(status.Conditions) != 1 || status.Conditions[0].Status != metav1.ConditionFalse || status.Conditions[0].Reason != progressDeadlineExceeded {
		t.Fatalf("condition = %+v", status.Conditions)
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
	status := deriveRevisionStatus(revision, []corev1.Pod{pod})
	if got := status.Conditions[0].Message; got != "pod dep-web-00001-0: ImagePullBackOff: image not found" {
		t.Fatalf("failure message = %q", got)
	}
}
