package kubernetes

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/pool"
	revisionapi "orchestrator/internal/revision"
	"orchestrator/internal/warm"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metameta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
)

const (
	revisionConditionReady = "Ready"
	// claimPollInterval paces the serving probe of a claimed pod whose
	// workload has not answered yet.
	claimPollInterval = time.Second
)

// revisionController owns warm process-lifetime informer caches. Only its
// queue workers are leader-gated, so a new leader never starts with cold API
// caches or needs per-Revision GET/LIST calls to audit the fleet.
type revisionController struct {
	o *Orchestrator

	revisionInformer cache.SharedIndexInformer
	revisionLister   cache.GenericLister
	podInformer      cache.SharedIndexInformer
	podLister        corelisters.PodLister

	mu                 sync.Mutex
	queue              workqueue.TypedRateLimitingInterface[string]
	pending            map[string]time.Time
	initial            map[string]struct{}
	convergenceStarted time.Time
}

func newRevisionController(o *Orchestrator) *revisionController {
	return &revisionController{o: o, pending: make(map[string]time.Time)}
}

// start launches and synchronizes informers for the process lifetime. Event
// handlers are active on followers, but enqueue only while this replica leads.
func (c *revisionController) start(ctx context.Context) error {
	revisionFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		c.o.revisions.Dynamic(), 0, c.o.namespace,
		func(opts *metav1.ListOptions) { opts.LabelSelector = LabelManagedBy + "=" + ManagedByValue },
	)
	revisions := revisionFactory.ForResource(revisionapi.Resource())
	c.revisionInformer = revisions.Informer()
	c.revisionLister = revisions.Lister()
	podFactory := informers.NewSharedInformerFactoryWithOptions(c.o.client, 0,
		informers.WithNamespace(c.o.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = LabelManagedBy + "=" + ManagedByValue + "," + LabelRevision
		}),
	)
	pods := podFactory.Core().V1().Pods()
	c.podInformer = pods.Informer()
	c.podLister = pods.Lister()

	_, _ = c.revisionInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueueRevisionObject,
		UpdateFunc: func(old, current any) {
			if shouldEnqueueRevisionUpdate(old, current) {
				c.enqueueRevisionObject(current)
			}
		},
		DeleteFunc: c.enqueueRevisionObject,
	})
	_, _ = c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.enqueuePodRevision,
		UpdateFunc: func(_, obj any) { c.enqueuePodRevision(obj) },
		DeleteFunc: c.enqueuePodRevision,
	})

	revisionFactory.Start(ctx.Done())
	podFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.revisionInformer.HasSynced, c.podInformer.HasSynced) {
		return errors.New("informer caches failed to sync")
	}
	if c.o.cfg.Metrics != nil {
		_ = c.o.cfg.Metrics.ObserveInt64("revision_queue_depth", "Revision reconcile items currently queued", c.queueDepth)
		_ = c.o.cfg.Metrics.ObserveInt64("revision_queue_oldest_age_seconds", "Age in seconds of the oldest queued Revision", c.oldestPendingSeconds)
		_ = c.o.cfg.Metrics.ObserveInt64("revision_replica_drift", "Total absolute desired-to-active replica drift across live Revisions", c.replicaDrift)
	}
	return nil
}

// runLeader owns one disposable queue for a leadership term. Followers keep
// caches warm; every acquisition enqueues the authoritative cached inventory.
func (c *revisionController) runLeader(ctx context.Context) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	// Install the queue before listing the store: an event landing between the
	// two would otherwise be dropped by add and missing from the snapshot.
	c.mu.Lock()
	c.queue = queue
	c.pending = make(map[string]time.Time)
	c.convergenceStarted = time.Now()
	objects := c.revisionInformer.GetStore().List()
	c.initial = make(map[string]struct{}, len(objects))
	for _, obj := range objects {
		if revision, ok := obj.(*unstructured.Unstructured); ok {
			c.initial[revision.GetName()] = struct{}{}
		}
	}
	c.mu.Unlock()

	for _, obj := range objects {
		c.enqueueRevisionObject(obj)
	}

	var workers sync.WaitGroup
	for range c.o.cfg.RevisionWorkers {
		workers.Go(func() {
			for c.processRevisionItem(ctx, queue) {
			}
		})
	}
	if len(objects) == 0 && c.o.cfg.Metrics != nil {
		c.o.cfg.Metrics.RecordRevisionLeaderConvergence(ctx, 0)
	}
	<-ctx.Done()
	c.mu.Lock()
	if c.queue == queue {
		c.queue = nil
	}
	c.mu.Unlock()
	queue.ShutDown()
	workers.Wait()
}

func (c *revisionController) enqueueRevisionObject(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if revision, ok := obj.(*unstructured.Unstructured); ok {
		c.add(revision.GetName(), time.Now())
	}
}

// enqueueRevisionUpdate ignores status-only writes made by this controller.
// Spec and /scale writes increment metadata.generation and must reconcile;
// feeding status updates back into the queue doubles work at fleet scale.
func shouldEnqueueRevisionUpdate(old, current any) bool {
	oldRevision, oldOK := old.(*unstructured.Unstructured)
	currentRevision, currentOK := current.(*unstructured.Unstructured)
	if !oldOK || !currentOK {
		return true
	}
	deletionStarted := oldRevision.GetDeletionTimestamp() == nil && currentRevision.GetDeletionTimestamp() != nil
	return currentRevision.GetGeneration() != oldRevision.GetGeneration() || deletionStarted
}

func (c *revisionController) enqueuePodRevision(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	if pod, ok := obj.(*corev1.Pod); ok {
		if revision := pod.Labels[LabelRevision]; revision != "" {
			c.add(objectNameFor(revision), time.Now())
		}
	}
}

func (c *revisionController) add(name string, at time.Time) {
	c.mu.Lock()
	queue := c.queue
	if queue != nil {
		// A deferred pass parks a future time here; a real event supersedes it.
		if existing, exists := c.pending[name]; !exists || existing.After(at) {
			c.pending[name] = at
		}
	}
	c.mu.Unlock()
	if queue != nil {
		queue.Add(name)
	}
}

func (c *revisionController) processRevisionItem(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string]) bool {
	name, shutdown := queue.Get()
	if shutdown {
		return false
	}
	defer queue.Done(name)
	c.mu.Lock()
	queuedAt := c.pending[name]
	delete(c.pending, name)
	c.mu.Unlock()
	if c.o.cfg.Metrics != nil && !queuedAt.IsZero() {
		c.o.cfg.Metrics.RecordRevisionQueueWait(ctx, time.Since(queuedAt).Seconds())
	}
	started := time.Now()
	err := c.reconcileCached(ctx, name, queuedAt)
	if c.o.cfg.Metrics != nil {
		c.o.cfg.Metrics.RecordRevisionReconcile(ctx, err == nil || apierrors.IsNotFound(err), time.Since(started).Seconds())
	}
	if err != nil {
		if !apierrors.IsNotFound(err) {
			slog.Warn("Revision reconcile failed", "revision", name, "error", err)
			c.mu.Lock()
			c.pending[name] = time.Now()
			c.mu.Unlock()
			queue.AddRateLimited(name)
			return true
		}
	}
	c.markInitialConverged(ctx, name)
	queue.Forget(name)
	// A Pending pod may emit no event at the exact progress deadline, and a
	// claimed pod's sidecar answering /ready is not a pod event at all. Arrange
	// one deferred reconcile instead of globally resyncing thousands of CRs.
	if delay := c.requeueDelay(name); delay > 0 {
		c.mu.Lock()
		c.pending[name] = time.Now().Add(delay)
		c.mu.Unlock()
		queue.AddAfter(name, delay)
	}
	return true
}

// requeueDelay is how long until the Revision needs another pass with no
// event to trigger it: the progress deadline, or the claim poll while a
// claimed pod has yet to serve.
func (c *revisionController) requeueDelay(name string) time.Duration {
	revision, err := c.revision(name)
	if err != nil {
		return 0
	}
	delay := revisionDeadlineDelay(revision)
	pods, err := c.podLister.Pods(c.o.namespace).List(labels.SelectorFromSet(labels.Set{LabelRevision: revision.Labels[LabelRevision]}))
	if err != nil {
		return delay
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil && claimedNotServing(pod) && (delay == 0 || claimPollInterval < delay) {
			return claimPollInterval
		}
	}
	return delay
}

func (c *revisionController) markInitialConverged(ctx context.Context, name string) {
	c.mu.Lock()
	if _, tracked := c.initial[name]; !tracked {
		c.mu.Unlock()
		return
	}
	delete(c.initial, name)
	done := len(c.initial) == 0
	duration := time.Since(c.convergenceStarted).Seconds()
	if done {
		c.initial = nil
	}
	c.mu.Unlock()
	if done && c.o.cfg.Metrics != nil {
		c.o.cfg.Metrics.RecordRevisionLeaderConvergence(ctx, duration)
	}
}

func (c *revisionController) queueDepth() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queue == nil {
		return 0
	}
	return int64(c.queue.Len())
}

func (c *revisionController) oldestPendingSeconds() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var oldest time.Duration
	for _, pendingAt := range c.pending {
		if age := now.Sub(pendingAt); age > oldest {
			oldest = age
		}
	}
	return int64(max(oldest, 0) / time.Second)
}

func (c *revisionController) replicaDrift() int64 {
	c.mu.Lock()
	leading := c.queue != nil
	c.mu.Unlock()
	if !leading {
		return 0
	}
	var total int64
	for _, obj := range c.revisionInformer.GetStore().List() {
		revision, ok := obj.(*unstructured.Unstructured)
		if !ok || revision.GetDeletionTimestamp() != nil {
			continue
		}
		desired, _, _ := unstructured.NestedInt64(revision.Object, "spec", "replicas")
		active, _, _ := unstructured.NestedInt64(revision.Object, "status", "replicas")
		if drift := desired - active; drift < 0 {
			total -= drift
		} else {
			total += drift
		}
	}
	return total
}

func (c *revisionController) revision(name string) (*revisionapi.Revision, error) {
	obj, err := c.revisionLister.ByNamespace(c.o.namespace).Get(name)
	if err != nil {
		return nil, err
	}
	unstructuredRevision, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("cached Revision %s has type %T", name, obj)
	}
	var revision revisionapi.Revision
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredRevision.Object, &revision); err != nil {
		return nil, err
	}
	return &revision, nil
}

func (c *revisionController) reconcileCached(ctx context.Context, name string, triggeredAt time.Time) error {
	revision, err := c.revision(name)
	if err != nil {
		return err
	}
	pods, err := c.podLister.Pods(c.o.namespace).List(labels.SelectorFromSet(labels.Set{
		LabelRevision: revision.Labels[LabelRevision],
	}))
	if err != nil {
		return err
	}
	return c.o.reconcileRevisionPods(ctx, revision, pods, false, triggeredAt)
}

// reconcileRevision converges one Revision using deterministic replica slots.
// Before Start, small embeddings and tests call it synchronously from Apply or
// Scale. In a running service, only the leader's informer worker writes pods.
func (o *Orchestrator) reconcileRevision(ctx context.Context, name string) error {
	revision, err := o.revisions.Get(ctx, o.namespace, name)
	if err != nil {
		return err
	}
	revisionName := revision.Labels[LabelRevision]
	podList, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{LabelRevision: revisionName}).String(),
	})
	if err != nil {
		return err
	}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}
	return o.reconcileRevisionPods(ctx, revision, pods, true, time.Now())
}

func (o *Orchestrator) reconcileRevisionPods(ctx context.Context, revision *revisionapi.Revision, pods []*corev1.Pod, direct bool, triggeredAt time.Time) error {
	desired, deleting := desiredRevisionReplicas(revision)
	state, stale := classifyRevisionPods(revision, pods)
	if stale {
		return nil
	}
	if err := o.deleteRevisionPods(ctx, append(state.terminal, state.invalid...), "terminal_or_invalid"); err != nil {
		return err
	}
	if err := o.scaleDownRevisionPods(ctx, state.active, desired, deleting); err != nil {
		return err
	}
	createErr := o.ensureRevisionPods(ctx, revision, state, desired, triggeredAt)
	if deleting {
		return nil
	}

	// Pre-Start synchronous callers refresh from the API immediately. Running
	// workers stay cache-only; Pod watch events supply the next status pass.
	if direct {
		podList, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelRevision + "=" + revision.Labels[LabelRevision],
		})
		if err != nil {
			return err
		}
		pods = pods[:0]
		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
	}
	podValues := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		podValues = append(podValues, *pod)
	}
	if err := o.updateRevisionStatus(ctx, revision, podValues, createErr); err != nil {
		return err
	}
	// A rejected create (quota, admission, a webhook) is reported through the
	// Ready condition, which the deployment surfaces as `failed` with the
	// message. Apply and Scale therefore still succeed, as they did when a
	// ReplicaSet absorbed the rejection; the leader worker returns the error
	// so the queue retries with backoff until the pod admits.
	if direct {
		return nil
	}
	return createErr
}

type revisionPodState struct {
	active      map[int]*corev1.Pod
	terminating map[int][]string
	terminal    []*corev1.Pod
	invalid     []*corev1.Pod
}

func desiredRevisionReplicas(revision *revisionapi.Revision) (int32, bool) {
	deleting := revision.DeletionTimestamp != nil
	desired := revision.Spec.Replicas
	if deleting || desired < 0 {
		// A Revision with a deletion timestamp (a finalizer holding it) is on
		// its way out; recreating its pods would only race the garbage collector.
		desired = 0
	}
	return desired, deleting
}

func classifyRevisionPods(revision *revisionapi.Revision, pods []*corev1.Pod) (revisionPodState, bool) {
	state := revisionPodState{active: make(map[int]*corev1.Pod), terminating: make(map[int][]string)}
	for _, pod := range pods {
		if podGeneration(pod) > revision.Generation {
			// Acting on a Pod from a newer spec would undo a concurrent update.
			return state, true
		}
		slot, err := strconv.Atoi(pod.Labels[LabelReplicaSlot])
		if err != nil || slot < 0 {
			state.invalid = append(state.invalid, pod)
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			state.terminal = append(state.terminal, pod)
			continue
		}
		if pod.DeletionTimestamp != nil {
			state.terminating[slot] = append(state.terminating[slot], pod.Name)
			continue
		}
		if existing := state.active[slot]; existing != nil {
			// Deterministically retain one slot owner after a race or mutation.
			if pod.Name < existing.Name {
				state.invalid = append(state.invalid, existing)
				state.active[slot] = pod
			} else {
				state.invalid = append(state.invalid, pod)
			}
			continue
		}
		state.active[slot] = pod
	}
	return state, false
}

func (o *Orchestrator) deleteRevisionPods(ctx context.Context, pods []*corev1.Pod, reason string) error {
	for _, pod := range pods {
		if err := o.client.CoreV1().Pods(o.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if o.cfg.Metrics != nil {
			o.cfg.Metrics.RecordRevisionPodDelete(ctx, reason)
		}
	}
	return nil
}

func (o *Orchestrator) scaleDownRevisionPods(ctx context.Context, active map[int]*corev1.Pod, desired int32, deleting bool) error {
	for slot, pod := range active {
		if int32(slot) < desired {
			continue
		}
		reason := "scale_down"
		if deleting {
			reason = "revision_deleting"
		}
		if err := o.deleteRevisionPods(ctx, []*corev1.Pod{pod}, reason); err != nil {
			return err
		}
		delete(active, slot)
	}
	return nil
}

func (o *Orchestrator) ensureRevisionPods(ctx context.Context, revision *revisionapi.Revision, state revisionPodState, desired int32, triggeredAt time.Time) error {
	for _, pod := range state.active {
		if claimedNotServing(pod) {
			if err := o.markServing(ctx, pod); err != nil {
				return err
			}
		}
	}
	for slot := range desired {
		if state.active[int(slot)] != nil {
			continue
		}
		created, err := o.ensureRevisionPod(ctx, revision, int(slot), state.terminating[int(slot)])
		if err != nil {
			return err
		}
		if created && o.cfg.Metrics != nil {
			o.cfg.Metrics.RecordRevisionPodCreate(ctx, time.Since(triggeredAt).Seconds())
		}
	}
	return nil
}

func (o *Orchestrator) ensureRevisionPod(ctx context.Context, revision *revisionapi.Revision, slot int, terminating []string) (bool, error) {
	if matchedPool := o.poolForRevision(revision); matchedPool != nil && revision.Spec.Claim != nil && o.pools != nil {
		if _, err := o.claimRevisionPod(ctx, revision, matchedPool, slot); err == nil {
			return true, nil
		} else if !errors.Is(err, apperrors.ErrExhausted) {
			return false, err
		}
		// Exhaustion of a transparent optimization falls back to the full
		// direct template retained on every Revision.
	}
	pod := buildRevisionPod(revision, slot, terminating)
	_, err := o.client.CoreV1().Pods(o.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	return err == nil, err
}

// updateRevisionStatus tolerates the intentional race between the synchronous
// Apply/Scale fast path and the leader worker observing the same event. On a
// conflict it refreshes the CR and re-derives status against the current spec,
// so an API request never fails merely because the background controller won.
func (o *Orchestrator) updateRevisionStatus(ctx context.Context, revision *revisionapi.Revision, pods []corev1.Pod, createErr error) error {
	candidate := revision
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		status := deriveRevisionStatus(candidate, pods, createErr)
		if reflect.DeepEqual(candidate.Status, status) {
			return nil
		}
		candidate.Status = status
		_, err := o.revisions.UpdateStatus(ctx, o.namespace, candidate)
		if apierrors.IsConflict(err) {
			latest, getErr := o.revisions.Get(ctx, o.namespace, candidate.Name)
			if getErr != nil {
				return getErr
			}
			candidate = latest
		}
		return err
	})
}

func buildRevisionPod(revision *revisionapi.Revision, slot int, terminating []string) *corev1.Pod {
	podLabels := mapsClone(revision.Spec.Template.Labels)
	podLabels[LabelReplicaSlot] = strconv.Itoa(slot)
	annotations := mapsClone(revision.Spec.Template.Annotations)
	annotations[AnnotationRevisionGeneration] = strconv.FormatInt(revision.Generation, 10)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podNameFor(revision.Name, slot, terminating),
			Namespace:   revision.Namespace,
			Labels:      podLabels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: revisionapi.APIVersion(), Kind: revisionapi.Kind,
				Name: revision.Name, UID: revision.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: *revision.Spec.Template.Spec.DeepCopy(),
	}
}

func (o *Orchestrator) claimRevisionPod(ctx context.Context, revision *revisionapi.Revision, p *pool.Pool, slot int) (_ *corev1.Pod, outcomeErr error) {
	claim := *revision.Spec.Claim
	claim.ClaimID = revisionClaimID(revision, slot)
	started := time.Now()
	if o.cfg.Metrics != nil {
		o.cfg.Metrics.RecordPoolClaimStarted(ctx, "revision", p.ID)
		defer func() {
			o.cfg.Metrics.RecordPoolClaimFinished(ctx, "revision", p.ID, outcomeErr == nil, time.Since(started).Seconds())
		}()
	}
	podLabels := revisionLabels(revision.Labels[LabelDeploymentID], revision.Labels[LabelRevision])
	podLabels[LabelReplicaSlot] = strconv.Itoa(slot)
	owners := []metav1.OwnerReference{{
		APIVersion: revisionapi.APIVersion(), Kind: revisionapi.Kind, Name: revision.Name, UID: revision.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
	pod, err := o.pools.Claim(ctx, p, &claim, warm.Binding{
		Spec: &claim, Labels: podLabels, Owners: owners,
	})
	if err != nil {
		return nil, err
	}
	return pod, o.markServing(ctx, pod)
}

// markServing stamps the serving gate once the claimed pod's sidecar answers
// /ready. A warm pod is kubelet-Ready before its claim, so pod readiness alone
// cannot admit it. One probe per pass, never a wait: a slow or broken workload
// must not hold a controller worker, and it is reported the same way as a
// direct pod's — through the Revision's progress deadline.
func (o *Orchestrator) markServing(ctx context.Context, pod *corev1.Pod) error {
	if !o.pools.Serving(ctx, pod) {
		return nil
	}
	patch := []byte(`{"metadata":{"labels":{"` + LabelServing + `":"true"}}}`)
	_, err := o.client.CoreV1().Pods(o.namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func claimedNotServing(pod *corev1.Pod) bool {
	return pod.Labels[LabelPoolClaim] != "" && pod.Labels[LabelServing] != "true"
}

func revisionClaimID(revision *revisionapi.Revision, slot int) string {
	seed := string(revision.UID)
	if seed == "" {
		seed = revision.Name
	}
	sum := sha256.Sum256([]byte(seed))
	return "rev-" + hex.EncodeToString(sum[:8]) + "-" + strconv.Itoa(slot)
}

func podGeneration(pod *corev1.Pod) int64 {
	generation, _ := strconv.ParseInt(pod.Annotations[AnnotationRevisionGeneration], 10, 64)
	return generation
}

// podNameFor names a slot's Pod deterministically, so the synchronous and
// cached reconcilers racing on one slot collide on AlreadyExists instead of
// producing duplicates. While the slot's previous Pods are still terminating
// their names are taken, so the replacement derives its suffix from that set:
// both writers still agree on the name, and the slot regains capacity at
// once, as under a ReplicaSet, rather than after the grace period (or never,
// when the node went away with the pod).
func podNameFor(revisionName string, slot int, terminating []string) string {
	suffix := "-" + strconv.Itoa(slot)
	if len(terminating) > 0 {
		slices.Sort(terminating)
		sum := sha256.Sum256([]byte(strings.Join(terminating, "\n")))
		suffix += "-" + hex.EncodeToString(sum[:3])
	}
	if len(revisionName)+len(suffix) <= 63 {
		return revisionName + suffix
	}
	sum := sha256.Sum256([]byte(revisionName))
	hash := hex.EncodeToString(sum[:4])
	prefixLen := 63 - len(suffix) - len(hash) - 1
	return strings.TrimRight(revisionName[:prefixLen], "-") + "-" + hash + suffix
}

func deriveRevisionStatus(revision *revisionapi.Revision, pods []corev1.Pod, createErr error) revisionapi.Status {
	status := revisionapi.Status{ObservedGeneration: revision.Generation}
	seen := make(map[int]bool)
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		slot, err := strconv.Atoi(pod.Labels[LabelReplicaSlot])
		if err != nil || slot < 0 || int32(slot) >= revision.Spec.Replicas || seen[slot] {
			continue
		}
		seen[slot] = true
		status.Replicas++
		if podReadyForRevision(pod) {
			status.ReadyReplicas++
		}
	}

	condition := metav1.Condition{Type: revisionConditionReady, ObservedGeneration: revision.Generation}
	switch {
	case revision.Spec.Replicas == 0:
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "ScaledToZero", "revision is scaled to zero"
	case status.ReadyReplicas >= revision.Spec.Replicas:
		condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, "PodsReady", "all desired pods are ready"
	case createErr != nil:
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "ReplicaFailure", createErr.Error()
	case revisionTimedOut(revision):
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, progressDeadlineExceeded, revisionFailureMessage(pods)
	default:
		condition.Status, condition.Reason, condition.Message = metav1.ConditionUnknown, "PodsNotReady", "waiting for desired pods to become ready"
	}
	status.Conditions = append([]metav1.Condition(nil), revision.Status.Conditions...)
	// A spec change restarts the progress clock: drop the condition written
	// for the old generation so its replacement gets a fresh transition time.
	if previous := metameta.FindStatusCondition(status.Conditions, revisionConditionReady); previous != nil && previous.ObservedGeneration != revision.Generation {
		metameta.RemoveStatusCondition(&status.Conditions, revisionConditionReady)
	}
	metameta.SetStatusCondition(&status.Conditions, condition)
	slices.SortFunc(status.Conditions, func(a, b metav1.Condition) int { return cmp.Compare(a.Type, b.Type) })
	return status
}

// readyDeadline is when the running progress clock expires. The clock starts
// when the Revision last stopped being ready under its current spec: the
// Ready condition's transition into Unknown, or creation before any status
// exists. It is never measured from creation for the Revision's whole life,
// which would fail a revision scaled up after an hour at zero on its first
// pass. No clock is running while the condition is True or False, or belongs
// to an older generation: the status write this pass makes starts one.
func readyDeadline(revision *revisionapi.Revision) (time.Time, bool) {
	if revision.Spec.ReadyTimeoutSeconds <= 0 {
		return time.Time{}, false
	}
	timeout := time.Duration(revision.Spec.ReadyTimeoutSeconds) * time.Second
	condition := metameta.FindStatusCondition(revision.Status.Conditions, revisionConditionReady)
	switch {
	case condition == nil:
		if revision.CreationTimestamp.IsZero() {
			return time.Time{}, false
		}
		return revision.CreationTimestamp.Add(timeout), true
	case condition.ObservedGeneration != revision.Generation, condition.Status != metav1.ConditionUnknown:
		return time.Time{}, false
	}
	return condition.LastTransitionTime.Add(timeout), true
}

func revisionTimedOut(revision *revisionapi.Revision) bool {
	// Once exceeded, the deadline stays exceeded until the pods actually
	// become ready or the spec changes; re-arming it would oscillate.
	if condition := metameta.FindStatusCondition(revision.Status.Conditions, revisionConditionReady); condition != nil &&
		condition.Reason == progressDeadlineExceeded && condition.ObservedGeneration == revision.Generation {
		return true
	}
	deadline, ok := readyDeadline(revision)
	return ok && !time.Now().Before(deadline)
}

// revisionDeadlineDelay is how long until the Revision needs a deadline pass.
// A deadline already behind us was written as exceeded by the pass that just
// ran; the informer cache merely has not caught up, so nothing is scheduled.
func revisionDeadlineDelay(revision *revisionapi.Revision) time.Duration {
	deadline, ok := readyDeadline(revision)
	if !ok {
		return 0
	}
	return max(time.Until(deadline), 0)
}

func revisionFailureMessage(pods []corev1.Pod) string {
	for i := range pods {
		for _, condition := range pods[i].Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Message != "" {
				return condition.Message
			}
		}
		for _, status := range append(pods[i].Status.InitContainerStatuses, pods[i].Status.ContainerStatuses...) {
			if status.State.Waiting != nil {
				return fmt.Sprintf("pod %s: %s: %s", pods[i].Name, status.State.Waiting.Reason, status.State.Waiting.Message)
			}
		}
	}
	return "revision did not become ready before its progress deadline"
}

func podReadyForRevision(pod *corev1.Pod) bool {
	if claimedNotServing(pod) {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func mapsClone[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in)+1)
	maps.Copy(out, in)
	return out
}
