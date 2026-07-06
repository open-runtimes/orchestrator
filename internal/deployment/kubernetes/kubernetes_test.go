package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

func newTestOrchestrator(t *testing.T) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	registerScaleSubresource(cs)
	cfg := Config{SidecarImage: "sidecar:latest", Namespace: "orchestrator", RunAsUser: 65532, GatewayEnabled: true}
	cfg.applyDefaults()
	return &Orchestrator{
		client:    cs,
		gateway:   gatewayfake.NewClientset(),
		namespace: cfg.Namespace,
		cfg:       cfg,
	}, cs
}

// registerScaleSubresource teaches the fake clientset the deployments/scale
// subresource, which it does not implement natively (GetScale/UpdateScale
// panic casting the tracked *apps/v1.Deployment to *autoscaling/v1.Scale).
func registerScaleSubresource(cs *fake.Clientset) {
	gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(gvr, get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		dep := obj.(*appsv1.Deployment)
		replicas := int32(1)
		if dep.Spec.Replicas != nil {
			replicas = *dep.Spec.Replicas
		}
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: dep.Namespace},
			Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
		}, nil
	})
	cs.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(k8stesting.UpdateAction)
		if !ok || action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := update.GetObject().(*autoscalingv1.Scale)
		obj, err := cs.Tracker().Get(gvr, update.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		dep := obj.(*appsv1.Deployment)
		dep.Spec.Replicas = &scale.Spec.Replicas
		if err := cs.Tracker().Update(gvr, dep, update.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, scale, nil
	})
}

func countActions(cs *fake.Clientset, verb, resource string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// mustApply applies the request and fails the test on error.
func mustApply(t *testing.T, o *Orchestrator, req *deployment.Request) {
	t.Helper()
	if _, err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// getMarkerData returns the marker ConfigMap for the deployment.
func getMarkerData(t *testing.T, o *Orchestrator, id string) marker {
	t.Helper()
	m, err := o.getMarker(t.Context(), id)
	if err != nil {
		t.Fatalf("getMarker(%s): %v", id, err)
	}
	return m
}

// getStoredSpec returns the head spec JSON off the dep-{id} Secret.
func getStoredSpec(t *testing.T, o *Orchestrator, id string) string {
	t.Helper()
	spec, err := o.getSpecJSON(t.Context(), id)
	if err != nil {
		t.Fatalf("getSpecJSON(%s): %v", id, err)
	}
	return spec
}

// getRoute fetches the deployment's HTTPRoute.
func getRoute(t *testing.T, o *Orchestrator, id string) *gatewayv1.HTTPRoute {
	t.Helper()
	route, err := o.gateway.GatewayV1().HTTPRoutes("orchestrator").Get(t.Context(), objectNameFor(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get HTTPRoute for %s: %v", id, err)
	}
	return route
}

// setAvailable marks a revision's Deployment as having n available replicas.
func setAvailable(t *testing.T, o *Orchestrator, rev string, n int32) {
	t.Helper()
	ctx := t.Context()
	dep, err := o.client.AppsV1().Deployments("orchestrator").Get(ctx, objectNameFor(rev), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get revision %s: %v", rev, err)
	}
	dep.Status.AvailableReplicas = n
	if _, err := o.client.AppsV1().Deployments("orchestrator").Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update revision %s: %v", rev, err)
	}
}

// setFailed marks a revision's Deployment as ProgressDeadlineExceeded.
func setFailed(t *testing.T, o *Orchestrator, rev, message string) {
	t.Helper()
	ctx := t.Context()
	dep, err := o.client.AppsV1().Deployments("orchestrator").Get(ctx, objectNameFor(rev), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get revision %s: %v", rev, err)
	}
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: progressDeadlineExceeded, Message: message,
	}}
	if _, err := o.client.AppsV1().Deployments("orchestrator").Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update revision %s: %v", rev, err)
	}
}

// --- Apply ---

func TestApply_FirstMintsRevision00001(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()

	mustApply(t, o, req)

	m := getMarkerData(t, o, "web")
	if m.LatestRevision != "web-00001" {
		t.Errorf("latestRevision: want web-00001, got %s", m.LatestRevision)
	}
	if m.LastReady != "" {
		t.Errorf("lastReady: want empty until the rollout reconciler cuts, got %s", m.LastReady)
	}
	if m.TrafficMode != trafficModeAuto {
		t.Errorf("trafficMode: want auto, got %s", m.TrafficMode)
	}
	if m.Host != "web.example.com" {
		t.Errorf("marker host: got %s", m.Host)
	}

	// The spec lives on the dep-{id} Secret (it carries callback keys), with
	// the managed-by + deployment.id labels, never on the marker ConfigMap.
	secret, err := cs.CoreV1().Secrets("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected spec Secret: %v", err)
	}
	if string(secret.Data[specSecretKey]) != mustSpecJSON(t, req) {
		t.Errorf("spec secret mismatch: %s", secret.Data[specSecretKey])
	}
	if secret.Labels[LabelManagedBy] != ManagedByValue || secret.Labels[LabelDeploymentID] != "web" {
		t.Errorf("spec secret labels: got %v", secret.Labels)
	}
	cm, err := cs.CoreV1().ConfigMaps("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected marker ConfigMap: %v", err)
	}
	if _, ok := cm.Data["spec"]; ok {
		t.Error("marker ConfigMap must not carry the spec (secret material)")
	}

	if _, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), "dep-web-00001", metav1.GetOptions{}); err != nil {
		t.Errorf("expected revision Deployment: %v", err)
	}
	if _, err := cs.CoreV1().Services("orchestrator").Get(t.Context(), "dep-web-00001", metav1.GetOptions{}); err != nil {
		t.Errorf("expected revision Service: %v", err)
	}

	route := getRoute(t, o, "web")
	if got := routeTargets(route); !reflect.DeepEqual(got, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("route targets: got %+v", got)
	}
	if len(route.Spec.Hostnames) != 1 || route.Spec.Hostnames[0] != "web.example.com" {
		t.Errorf("route hostnames: got %v", route.Spec.Hostnames)
	}
}

func TestApply_IdenticalSpecIsNoOp(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()

	mustApply(t, o, req)
	mustApply(t, o, req)

	if n := countActions(cs, "update", "deployments"); n != 0 {
		t.Errorf("identical spec must not update any Deployment, got %d updates", n)
	}
	// The compare runs against the spec Secret and must not rewrite it.
	if n := countActions(cs, "update", "secrets"); n != 0 {
		t.Errorf("identical spec must not rewrite the spec Secret, got %d updates", n)
	}
	m := getMarkerData(t, o, "web")
	if m.LatestRevision != "web-00001" {
		t.Errorf("identical spec must not mint a revision, got %s", m.LatestRevision)
	}
}

func TestApply_MissingSpecSecretHealsByMinting(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	mustApply(t, o, req)

	if err := cs.CoreV1().Secrets("orchestrator").Delete(t.Context(), "dep-web", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete spec secret: %v", err)
	}

	// Marker present, secret gone: Apply heals by minting a fresh head, so
	// the stored spec always describes latestRevision.
	mustApply(t, o, req)
	if got := getStoredSpec(t, o, "web"); got != mustSpecJSON(t, req) {
		t.Errorf("spec secret not recreated: %s", got)
	}
	if m := getMarkerData(t, o, "web"); m.LatestRevision != "web-00002" {
		t.Errorf("heal must mint the next revision, got %s", m.LatestRevision)
	}
}

func TestApply_ChangedSpecMintsNextRevisionTrafficUntouched(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())

	changed := testRequest()
	changed.Environment["FOO"] = "changed"
	mustApply(t, o, changed)

	m := getMarkerData(t, o, "web")
	if m.LatestRevision != "web-00002" {
		t.Fatalf("latestRevision: want web-00002, got %s", m.LatestRevision)
	}
	if getStoredSpec(t, o, "web") != mustSpecJSON(t, changed) {
		t.Error("spec secret not replaced on mint")
	}
	if m.LastReady != "" {
		t.Errorf("lastReady must be untouched by Apply, got %s", m.LastReady)
	}
	if _, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), "dep-web-00002", metav1.GetOptions{}); err != nil {
		t.Errorf("expected revision 00002 Deployment: %v", err)
	}
	// The old revision's objects remain (rollback material).
	if _, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), "dep-web-00001", metav1.GetOptions{}); err != nil {
		t.Errorf("revision 00001 must be retained: %v", err)
	}
	// Traffic is untouched: still 100% on 00001 until the auto-cut.
	got := routeTargets(getRoute(t, o, "web"))
	if !reflect.DeepEqual(got, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("route targets must be untouched by mint: %+v", got)
	}
}

func TestApply_HealsMissingServiceAndRoute(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	mustApply(t, o, req)

	if err := cs.CoreV1().Services("orchestrator").Delete(t.Context(), "dep-web-00001", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete Service: %v", err)
	}
	if err := o.gateway.GatewayV1().HTTPRoutes("orchestrator").Delete(t.Context(), "dep-web", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete HTTPRoute: %v", err)
	}

	// Identical spec: workload untouched, but Service + route are healed.
	mustApply(t, o, req)
	if _, err := cs.CoreV1().Services("orchestrator").Get(t.Context(), "dep-web-00001", metav1.GetOptions{}); err != nil {
		t.Errorf("expected Service recreated: %v", err)
	}
	getRoute(t, o, "web")
}

func TestApply_GatewayDisabledSkipsRoute(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	o.cfg.GatewayEnabled = false

	mustApply(t, o, testRequest())

	routes, err := o.gateway.GatewayV1().HTTPRoutes("orchestrator").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	if len(routes.Items) != 0 {
		t.Errorf("gateway disabled must not write HTTPRoutes, got %d", len(routes.Items))
	}
	// Status still works off the marker fallback.
	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !reflect.DeepEqual(s.Traffic, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("fallback traffic: got %+v", s.Traffic)
	}
}

// --- Delete ---

func TestDelete_FullTeardown(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	changed := testRequest()
	changed.Environment["FOO"] = "v2"
	mustApply(t, o, changed)

	if err := o.Delete(t.Context(), "web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for _, rev := range []string{"web-00001", "web-00002"} {
		if _, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), objectNameFor(rev), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("expected Deployment %s deleted, got err=%v", rev, err)
		}
		if _, err := cs.CoreV1().Services("orchestrator").Get(t.Context(), objectNameFor(rev), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("expected Service %s deleted, got err=%v", rev, err)
		}
	}
	if _, err := cs.CoreV1().ConfigMaps("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected marker deleted, got err=%v", err)
	}
	if _, err := cs.CoreV1().Secrets("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected spec secret deleted, got err=%v", err)
	}
	if _, err := o.gateway.GatewayV1().HTTPRoutes("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected HTTPRoute deleted, got err=%v", err)
	}

	if err := o.Delete(t.Context(), "web"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("second Delete: want NotFound, got %v", err)
	}
}

// --- Spec ---

func TestSpec_MarkerRoundTrip(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	req := testRequest()
	req.Probes = &deployment.Probes{Readiness: &deployment.Probe{Path: "/ready", PeriodMillis: 100}}
	mustApply(t, o, req)

	got, err := o.Spec(t.Context(), "web")
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != mustSpecJSON(t, req) {
		t.Errorf("round-trip mismatch:\nwant %s\ngot  %s", mustSpecJSON(t, req), gotJSON)
	}
}

func TestSpec_NotFound(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	if _, err := o.Spec(t.Context(), "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("want NotFound, got %v", err)
	}
}

func TestSpec_MissingSecretIsInternalNotNotFound(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	if err := cs.CoreV1().Secrets("orchestrator").Delete(t.Context(), "dep-web", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete spec secret: %v", err)
	}

	// The marker (identity anchor) exists, so this is corruption — a clear
	// Internal error naming the missing Secret, never NotFound.
	_, err := o.Spec(t.Context(), "web")
	if err == nil || errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("want an Internal error, got %v", err)
	}
	if !strings.Contains(err.Error(), "dep-web") {
		t.Errorf("error should name the missing Secret: %v", err)
	}
}

// --- Endpoints ---

func TestEndpoints_ReadyPodsOfRoutedRevisions(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())

	pods := []*corev1.Pod{
		testPod("routed-ready", "web", "web-00001", "10.0.0.1", corev1.ConditionTrue),
		testPod("routed-unready", "web", "web-00001", "10.0.0.2", corev1.ConditionFalse),
		testPod("routed-no-ip", "web", "web-00001", "", corev1.ConditionTrue),
		testPod("unrouted-revision", "web", "web-00002", "10.0.0.3", corev1.ConditionTrue),
		testPod("other-deployment", "other", "other-00001", "10.0.0.4", corev1.ConditionTrue),
	}
	for _, p := range pods {
		if _, err := cs.CoreV1().Pods("orchestrator").Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod %s: %v", p.Name, err)
		}
	}

	endpoints, err := o.Endpoints(t.Context(), "web")
	if err != nil {
		t.Fatalf("Endpoints: %v", err)
	}
	if len(endpoints) != 1 {
		t.Fatalf("want 1 endpoint, got %d: %v", len(endpoints), endpoints)
	}
	if endpoints[0].String() != "http://10.0.0.1:8000" {
		t.Errorf("endpoint: want http://10.0.0.1:8000, got %s", endpoints[0])
	}
}

// --- Scale ---

func TestScale_RoutedRevisionToZeroAndBack(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	ctx := t.Context()
	mustApply(t, o, testRequest())

	if err := o.Scale(ctx, "web", 0); err != nil {
		t.Fatalf("Scale(0): %v", err)
	}
	dep, err := cs.AppsV1().Deployments("orchestrator").Get(ctx, "dep-web-00001", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		t.Fatalf("replicas after Scale(0): got %v, want 0", dep.Spec.Replicas)
	}

	// Idempotent: same value performs no write.
	writes := countActions(cs, "update", "deployments")
	if err := o.Scale(ctx, "web", 0); err != nil {
		t.Fatalf("Scale(0) again: %v", err)
	}
	if countActions(cs, "update", "deployments") != writes {
		t.Fatal("idempotent Scale performed a write")
	}

	if err := o.Scale(ctx, "web", 2); err != nil {
		t.Fatalf("Scale(2): %v", err)
	}
	dep, _ = cs.AppsV1().Deployments("orchestrator").Get(ctx, "dep-web-00001", metav1.GetOptions{})
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("replicas after Scale(2): got %v, want 2", dep.Spec.Replicas)
	}
}

func TestScale_SplitTargetsLatestReady(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	ctx := t.Context()
	twoRevisions(t, o) // 00001 cut (lastReady), 00002 minted

	// Pin a 50/50 split; the routed-revision pick under a split is lastReady.
	if err := o.SetTraffic(ctx, "web", []deployment.Target{
		{RevisionName: "web-00001", Percent: 50},
		{RevisionName: "web-00002", Percent: 50},
	}); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}

	if err := o.Scale(ctx, "web", 3); err != nil {
		t.Fatalf("Scale(3): %v", err)
	}
	dep, err := cs.AppsV1().Deployments("orchestrator").Get(ctx, "dep-web-00001", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("split Scale must target the latest-ready revision (00001), got %v", dep.Spec.Replicas)
	}
}

func TestScale_NotFound(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	err := o.Scale(t.Context(), "ghost", 0)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error, got %v", err)
	}
}

// --- PodDisruptionBudget lifecycle ---

func TestApply_PDBCreatedForMultiReplica(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest()) // Replicas: 2

	pdb, err := cs.PolicyV1().PodDisruptionBudgets("orchestrator").Get(t.Context(), "dep-web-00001", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected revision PDB: %v", err)
	}
	// Belt-and-braces ownerReference on the revision Deployment.
	if len(pdb.OwnerReferences) != 1 || pdb.OwnerReferences[0].Kind != "Deployment" || pdb.OwnerReferences[0].Name != "dep-web-00001" {
		t.Errorf("ownerReferences: got %+v", pdb.OwnerReferences)
	}
}

func TestApply_NoPDBForSingleReplica(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()
	req.Replicas = 1
	mustApply(t, o, req)

	if _, err := cs.PolicyV1().PodDisruptionBudgets("orchestrator").Get(t.Context(), "dep-web-00001", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("single replica must not get a PDB (minAvailable:1 would deadlock drains), got err=%v", err)
	}
}

func TestDelete_RemovesPDBs(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	twoRevisions(t, o) // Replicas: 2 → both revisions carry PDBs

	if _, err := cs.PolicyV1().PodDisruptionBudgets("orchestrator").Get(t.Context(), "dep-web-00002", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected PDB for revision 00002: %v", err)
	}
	if err := o.Delete(t.Context(), "web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, rev := range []string{"web-00001", "web-00002"} {
		if _, err := cs.PolicyV1().PodDisruptionBudgets("orchestrator").Get(t.Context(), objectNameFor(rev), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("expected PDB %s deleted, got err=%v", rev, err)
		}
	}
}

// --- Ready / Start / Close ---

func TestReadyStartClose(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	o.cfg.GatewayEnabled = false // Start would launch informers against the fake otherwise

	if err := o.Ready(t.Context()); err != nil {
		t.Errorf("Ready: %v", err)
	}
	if err := o.Start(t.Context()); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := o.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestRunLeaderElected_DisabledRunsDirectly is the Start-under-election smoke
// for the disabled (single-replica) path: run executes synchronously, no
// Lease objects involved.
func TestRunLeaderElected_DisabledRunsDirectly(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)

	ran := false
	o.RunLeaderElected(t.Context(), func(context.Context) { ran = true })
	if !ran {
		t.Error("RunLeaderElected with election disabled must call run directly")
	}
	leases, err := cs.CoordinationV1().Leases("orchestrator").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list leases: %v", err)
	}
	if len(leases.Items) != 0 {
		t.Errorf("election disabled must not touch Leases, got %d", len(leases.Items))
	}
}

func TestStart_SurfacesListErrors(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	cs.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if err := o.Start(t.Context()); err == nil {
		t.Error("want error when the API is unreachable")
	}
	_ = o.Close()
}

// --- helpers ---

func testPod(name, deploymentID, revision, ip string, ready corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: revisionLabels(deploymentID, revision),
		},
		Status: corev1.PodStatus{
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
		},
	}
}

// twoRevisions applies rev 00001, lets the rollout reconciler cut to it, then
// mints rev 00002 — the canonical mid-rollout fixture.
func twoRevisions(t *testing.T, o *Orchestrator) {
	t.Helper()
	mustApply(t, o, testRequest())
	setAvailable(t, o, "web-00001", 1)
	reconcileAll(t, o)

	changed := testRequest()
	changed.Environment["FOO"] = "v2"
	mustApply(t, o, changed)
}

// reconcileAll runs one rollout reconciler sweep.
func reconcileAll(t *testing.T, o *Orchestrator) {
	t.Helper()
	o.reconcileRollouts(t.Context())
}
