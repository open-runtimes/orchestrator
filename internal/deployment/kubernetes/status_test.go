package kubernetes

import (
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStatus_NotFound(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	if _, err := o.Status(t.Context(), "ghost"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("want NotFound, got %v", err)
	}
}

func TestStatus_PendingThenReady(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	mustApply(t, o, testRequest()) // replicas: 2

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StatePending {
		t.Errorf("fresh revision: want pending, got %s", s.State)
	}
	if s.DesiredReplicas != 2 || s.AvailableReplicas != 0 {
		t.Errorf("counts: got %+v", s)
	}
	if !reflect.DeepEqual(s.Revisions, []string{"web-00001"}) {
		t.Errorf("revisions: got %v", s.Revisions)
	}
	if !reflect.DeepEqual(s.Traffic, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("traffic: got %+v", s.Traffic)
	}

	setAvailable(t, o, "web-00001", 2)
	s, err = o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StateReady || s.AvailableReplicas != 2 {
		t.Errorf("want ready 2/2, got %s %d/%d", s.State, s.AvailableReplicas, s.DesiredReplicas)
	}
}

func TestStatus_DegradedWhenPartiallyAvailable(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	mustApply(t, o, testRequest()) // replicas: 2
	setAvailable(t, o, "web-00001", 1)

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StateDegraded {
		t.Errorf("want degraded, got %s", s.State)
	}
}

func TestStatus_IdleWhenRoutedScaledToZero(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	if err := o.Scale(t.Context(), "web", 0); err != nil {
		t.Fatalf("Scale(0): %v", err)
	}

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StateIdle {
		t.Errorf("want idle, got %s", s.State)
	}
}

// TestStatus_StuckRolloutIsFailed: a failed NEW revision must surface as
// failed even though traffic still flows to the healthy old revision — the
// stuck rollout has to be visible.
func TestStatus_StuckRolloutIsFailed(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)
	setFailed(t, o, "web-00002", "deadline exceeded")
	reconcileAll(t, o) // must not cut

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StateFailed {
		t.Errorf("stuck rollout: want failed, got %s", s.State)
	}
	if s.Error != "deadline exceeded" {
		t.Errorf("want the controller's condition message, got %q", s.Error)
	}
	// Old revision still routed and healthy underneath.
	if !reflect.DeepEqual(s.Traffic, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("traffic: got %+v", s.Traffic)
	}
	if s.AvailableReplicas < 1 {
		t.Errorf("old revision capacity should still show: %+v", s)
	}
	if !reflect.DeepEqual(s.Revisions, []string{"web-00002", "web-00001"}) {
		t.Errorf("revisions newest-first: got %v", s.Revisions)
	}
}

func TestStatus_FirstRevisionFailedIsFailed(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	setFailed(t, o, "web-00001", "ImagePullBackOff")

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StateFailed || s.Error != "ImagePullBackOff" {
		t.Errorf("want failed/ImagePullBackOff, got %s/%q", s.State, s.Error)
	}
}

func TestStatus_DeletingWinsOverReady(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	setAvailable(t, o, "web-00001", 2)

	cm, err := cs.CoreV1().ConfigMaps("orchestrator").Get(t.Context(), "dep-web", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get marker: %v", err)
	}
	now := metav1.Now()
	cm.DeletionTimestamp = &now
	cm.Finalizers = []string{"orchestrator.dev/test"}
	if _, err := cs.CoreV1().ConfigMaps("orchestrator").Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update marker: %v", err)
	}

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.State != deployment.StateDeleting {
		t.Errorf("want deleting, got %s", s.State)
	}
}

func TestStatus_SplitSumsRoutedReplicas(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)
	setAvailable(t, o, "web-00002", 1)
	if err := o.SetTraffic(t.Context(), "web", canarySplit()); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}

	s, err := o.Status(t.Context(), "web")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// Both revisions weighted: desired 2+2, available 1+1.
	if s.DesiredReplicas != 4 || s.AvailableReplicas != 2 {
		t.Errorf("summed counts: got %d/%d", s.AvailableReplicas, s.DesiredReplicas)
	}
	if s.State != deployment.StateDegraded {
		t.Errorf("want degraded, got %s", s.State)
	}
	if !reflect.DeepEqual(s.Traffic, canarySplit()) {
		t.Errorf("traffic: got %+v", s.Traffic)
	}
}

// --- List ---

func TestList_ManagedOnly(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t)

	a := testRequest()
	b := testRequest()
	b.ID = "api"
	b.Hosts = []string{"api.example.com"}
	for _, req := range []*deployment.Request{a, b} {
		mustApply(t, o, req)
	}
	// An unmanaged ConfigMap in the namespace must not show up.
	unmanaged := getMarkerData(t, o, "web").configMap()
	unmanaged.Name = "other"
	unmanaged.Labels = nil
	if _, err := cs.CoreV1().ConfigMaps("orchestrator").Create(t.Context(), unmanaged, metav1.CreateOptions{}); err != nil {
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
