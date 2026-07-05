package kubernetes

import (
	"encoding/json"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
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
)

func newTestOrchestrator(t *testing.T) (*Orchestrator, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset()
	registerScaleSubresource(cs)
	cfg := Config{SidecarImage: "sidecar:latest", Namespace: "orchestrator", RunAsUser: 65532}
	return &Orchestrator{client: cs, namespace: cfg.Namespace, cfg: cfg}, cs
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

// --- Apply ---

func TestApply_CreatesDeploymentAndService(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()

	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	dep, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected Deployment to exist: %v", err)
	}
	if dep.Annotations[AnnotationSpec] != mustSpecJSON(t, req) {
		t.Errorf("spec annotation mismatch: %s", dep.Annotations[AnnotationSpec])
	}
	if _, err := cs.CoreV1().Services("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); err != nil {
		t.Errorf("expected Service to exist: %v", err)
	}
}

func TestApply_IdenticalSpecIsNoOp(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()

	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	if n := countActions(cs, "update", "deployments"); n != 0 {
		t.Errorf("identical spec must not update the Deployment, got %d updates", n)
	}
	if n := countActions(cs, "create", "deployments"); n != 1 {
		t.Errorf("want exactly 1 Deployment create, got %d", n)
	}
}

func TestApply_ChangedSpecUpdatesInPlace(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()

	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	changed := testRequest()
	changed.Environment["FOO"] = "changed"
	if err := o.Apply(t.Context(), changed); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	if n := countActions(cs, "update", "deployments"); n != 1 {
		t.Fatalf("want 1 Deployment update, got %d", n)
	}
	dep, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if dep.Annotations[AnnotationSpec] != mustSpecJSON(t, changed) {
		t.Error("spec annotation not replaced on update")
	}
}

func TestApply_RecreatesMissingService(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	req := testRequest()

	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := cs.CoreV1().Services("orchestrator").Delete(t.Context(), "dep-web", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete Service: %v", err)
	}

	// Identical spec: Deployment untouched, but the Service is healed.
	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if _, err := cs.CoreV1().Services("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); err != nil {
		t.Errorf("expected Service recreated: %v", err)
	}
}

// --- Delete ---

func TestDelete_RemovesObjects(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)

	if err := o.Apply(t.Context(), testRequest()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := o.Delete(t.Context(), "web"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := cs.AppsV1().Deployments("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Deployment deleted, got err=%v", err)
	}
	if _, err := cs.CoreV1().Services("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected Service deleted, got err=%v", err)
	}

	if err := o.Delete(t.Context(), "web"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("second Delete: want NotFound, got %v", err)
	}
}

// --- Status ---

func TestStatus_NotFound(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	if _, err := o.Status(t.Context(), "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("want NotFound, got %v", err)
	}
}

func TestStatus_ReadyFromCluster(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)

	dep := buildDeployment(testRequest(), o.cfg, "{}")
	dep.Status = appsv1.DeploymentStatus{AvailableReplicas: 2}
	if _, err := cs.AppsV1().Deployments("orchestrator").Create(t.Context(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed Deployment: %v", err)
	}

	status, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.State != deployment.StateReady {
		t.Errorf("state: want ready, got %s", status.State)
	}
	if status.ID != "web" || status.DesiredReplicas != 2 || status.AvailableReplicas != 2 {
		t.Errorf("counts: got %+v", status)
	}
}

func TestDeriveStatus(t *testing.T) {
	t.Parallel()
	two := int32(2)
	now := metav1.Now()

	cases := []struct {
		name      string
		replicas  *int32
		status    appsv1.DeploymentStatus
		deleting  bool
		wantState string
		wantError string
	}{
		{
			name:     "ready when available meets desired",
			replicas: &two, status: appsv1.DeploymentStatus{AvailableReplicas: 2},
			wantState: deployment.StateReady,
		},
		{
			name:      "ready with nil replicas defaults desired to 1",
			status:    appsv1.DeploymentStatus{AvailableReplicas: 1},
			wantState: deployment.StateReady,
		},
		{
			name:     "degraded when partially available",
			replicas: &two, status: appsv1.DeploymentStatus{AvailableReplicas: 1},
			wantState: deployment.StateDegraded,
		},
		{
			name:     "pending while progressing",
			replicas: &two,
			status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetCreated",
			}}},
			wantState: deployment.StatePending,
		},
		{
			name:     "failed past progress deadline",
			replicas: &two,
			status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: "deadline exceeded",
			}}},
			wantState: deployment.StateFailed, wantError: "deadline exceeded",
		},
		{
			name:     "failed on replica failure",
			replicas: &two,
			status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: "quota exceeded",
			}}},
			wantState: deployment.StateFailed, wantError: "quota exceeded",
		},
		{
			name:     "deleting wins over ready",
			replicas: &two, status: appsv1.DeploymentStatus{AvailableReplicas: 2}, deleting: true,
			wantState: deployment.StateDeleting,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "dep-web",
					Labels: map[string]string{LabelDeploymentID: "web"},
				},
				Spec:   appsv1.DeploymentSpec{Replicas: tc.replicas},
				Status: tc.status,
			}
			if tc.deleting {
				dep.DeletionTimestamp = &now
			}

			got := deriveStatus(dep)
			if got.State != tc.wantState {
				t.Errorf("state: want %s, got %s", tc.wantState, got.State)
			}
			if got.Error != tc.wantError {
				t.Errorf("error: want %q, got %q", tc.wantError, got.Error)
			}
			if got.ID != "web" {
				t.Errorf("id: got %s", got.ID)
			}
		})
	}
}

// --- List ---

func TestList_ManagedOnly(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)

	a := testRequest()
	b := testRequest()
	b.ID = "api"
	for _, req := range []*deployment.Request{a, b} {
		if err := o.Apply(t.Context(), req); err != nil {
			t.Fatalf("Apply %s: %v", req.ID, err)
		}
	}
	// An unmanaged Deployment in the namespace must not show up.
	unmanaged := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	if _, err := cs.AppsV1().Deployments("orchestrator").Create(t.Context(), unmanaged, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed unmanaged: %v", err)
	}

	statuses, err := o.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("want 2 managed deployments, got %d", len(statuses))
	}
	ids := map[string]bool{}
	for _, s := range statuses {
		ids[s.ID] = true
	}
	if !ids["web"] || !ids["api"] {
		t.Errorf("ids: got %v", ids)
	}
}

// --- Spec ---

func TestSpec_RoundTrip(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	req := testRequest()
	req.Probes = &deployment.Probes{Readiness: &deployment.Probe{Path: "/ready", PeriodMillis: 100}}

	if err := o.Apply(t.Context(), req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

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

// --- Endpoints ---

func TestEndpoints_ReadyPodsOnly(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)

	pods := []*corev1.Pod{
		testPod("web-ready", "web", "10.0.0.1", corev1.ConditionTrue),
		testPod("web-unready", "web", "10.0.0.2", corev1.ConditionFalse),
		testPod("web-no-ip", "web", "", corev1.ConditionTrue),
		testPod("other-ready", "other", "10.0.0.3", corev1.ConditionTrue),
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

// --- Ready / Start / Close ---

func TestReadyStartClose(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)

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

func TestStart_SurfacesListErrors(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	cs.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if err := o.Start(t.Context()); err == nil {
		t.Error("want error when the API is unreachable")
	}
}

// --- helpers ---

func testPod(name, deploymentID, ip string, ready corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{LabelDeploymentID: deploymentID},
		},
		Status: corev1.PodStatus{
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}},
		},
	}
}

func TestScale_ToZeroAndBack(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	ctx := t.Context()

	req := &deployment.Request{ID: "web", Image: "nginx", Port: 8080, Replicas: 1}
	if err := o.Apply(ctx, req); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if err := o.Scale(ctx, "web", 0); err != nil {
		t.Fatalf("Scale(0): %v", err)
	}
	dep, err := cs.AppsV1().Deployments("orchestrator").Get(ctx, "dep-web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		t.Fatalf("replicas after Scale(0): got %v, want 0", dep.Spec.Replicas)
	}
	if got := deriveStatus(dep).State; got != deployment.StateIdle {
		t.Fatalf("state after Scale(0): got %s, want idle", got)
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
	dep, _ = cs.AppsV1().Deployments("orchestrator").Get(ctx, "dep-web", metav1.GetOptions{})
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 2 {
		t.Fatalf("replicas after Scale(2): got %v, want 2", dep.Spec.Replicas)
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
