package kubernetes

import (
	"fmt"
	"orchestrator/internal/deployment"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRollout_CutsToLatestWhenAvailable(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o) // lastReady=00001, latest=00002

	setAvailable(t, o, "web-00002", 1)
	reconcileAll(t, o)

	m := getMarkerData(t, o, "web")
	if m.LastReady != "web-00002" {
		t.Errorf("lastReady: want web-00002 after cut, got %s", m.LastReady)
	}
	if m.TrafficMode != trafficModeAuto {
		t.Errorf("auto-cut must keep auto mode, got %s", m.TrafficMode)
	}
	got := routeTargets(getRoute(t, o, "web"))
	if !reflect.DeepEqual(got, []deployment.Target{{RevisionName: "web-00002", Percent: 100}}) {
		t.Errorf("route after cut: got %+v", got)
	}
}

func TestRollout_DoesNotCutWhenUnavailable(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o) // 00002 has zero available replicas

	reconcileAll(t, o)

	m := getMarkerData(t, o, "web")
	if m.LastReady != "web-00001" {
		t.Errorf("lastReady must stay web-00001 while 00002 is unready, got %s", m.LastReady)
	}
	got := routeTargets(getRoute(t, o, "web"))
	if !reflect.DeepEqual(got, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("traffic must keep flowing to the old revision: %+v", got)
	}
}

func TestRollout_FailedRevisionNeverReceivesTraffic(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)
	setFailed(t, o, "web-00002", "ProgressDeadlineExceeded")

	reconcileAll(t, o)

	m := getMarkerData(t, o, "web")
	if m.LastReady != "web-00001" {
		t.Errorf("a failed new revision must never be cut to, lastReady=%s", m.LastReady)
	}
	got := routeTargets(getRoute(t, o, "web"))
	if !reflect.DeepEqual(got, []deployment.Target{{RevisionName: "web-00001", Percent: 100}}) {
		t.Errorf("traffic must remain on web-00001: %+v", got)
	}
}

func TestRollout_ManualModeIsLeftAlone(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)
	if err := o.SetTraffic(t.Context(), "web", canarySplit()); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}

	setAvailable(t, o, "web-00002", 1)
	reconcileAll(t, o)

	m := getMarkerData(t, o, "web")
	if m.TrafficMode != trafficModeManual || m.LastReady != "web-00001" {
		t.Errorf("manual mode must not auto-cut: mode=%s lastReady=%s", m.TrafficMode, m.LastReady)
	}
	if got := routeTargets(getRoute(t, o, "web")); !reflect.DeepEqual(got, canarySplit()) {
		t.Errorf("pinned split must be untouched: %+v", got)
	}
}

func TestRollout_ReArmedAutoCutsAfterManualPin(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)
	setAvailable(t, o, "web-00002", 1)

	// Reset to 100% latest re-arms auto; the next sweep records lastReady.
	if err := o.SetTraffic(t.Context(), "web", []deployment.Target{{RevisionName: "web-00002", Percent: 100}}); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}
	reconcileAll(t, o)

	if m := getMarkerData(t, o, "web"); m.LastReady != "web-00002" {
		t.Errorf("re-armed auto must cut: lastReady=%s", m.LastReady)
	}
}

func TestRetire_KeepsLimitRoutedAndLastReady(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	o.cfg.RevisionHistoryLimit = 2
	ctx := t.Context()

	// Mint five revisions, cutting to each in turn.
	req := testRequest()
	mustApply(t, o, req)
	for i := 1; i <= 5; i++ {
		if i > 1 {
			req = testRequest()
			req.Environment["FOO"] = fmt.Sprintf("v%d", i)
			mustApply(t, o, req)
		}
		setAvailable(t, o, revisionName("web", i), 1)
		reconcileAll(t, o)
	}

	revs, err := o.revisionNames(ctx, "web")
	if err != nil {
		t.Fatalf("revisionNames: %v", err)
	}
	if !reflect.DeepEqual(revs, []string{"web-00005", "web-00004"}) {
		t.Errorf("want newest 2 kept, got %v", revs)
	}

	// Pin traffic onto the oldest survivor, mint + cut again: the routed and
	// last-ready revisions must survive retirement even beyond the limit.
	if err := o.SetTraffic(ctx, "web", []deployment.Target{
		{RevisionName: "web-00004", Percent: 50},
		{RevisionName: "web-00005", Percent: 50},
	}); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}
	req = testRequest()
	req.Environment["FOO"] = "v6"
	mustApply(t, o, req)
	setAvailable(t, o, "web-00006", 1)
	// Manual mode: no cut. Re-arm auto and mint one more to force retire past
	// the routed set.
	if err := o.SetTraffic(ctx, "web", []deployment.Target{{RevisionName: "web-00006", Percent: 100}}); err != nil {
		t.Fatalf("SetTraffic reset: %v", err)
	}
	reconcileAll(t, o) // cuts to 00006, retires beyond limit 2

	revs, err = o.revisionNames(ctx, "web")
	if err != nil {
		t.Fatalf("revisionNames: %v", err)
	}
	if !reflect.DeepEqual(revs, []string{"web-00006", "web-00005"}) {
		t.Errorf("want [web-00006 web-00005], got %v", revs)
	}
}

func TestRetire_NeverDeletesWeightedRevision(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	o.cfg.RevisionHistoryLimit = 1
	ctx := t.Context()

	twoRevisions(t, o)
	// Split so 00001 keeps weight, then cut... manual mode blocks the cut, so
	// exercise retire directly.
	if err := o.SetTraffic(ctx, "web", canarySplit()); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}
	m := getMarkerData(t, o, "web")
	if err := o.retire(ctx, m); err != nil {
		t.Fatalf("retire: %v", err)
	}

	revs, err := o.revisionNames(ctx, "web")
	if err != nil {
		t.Fatalf("revisionNames: %v", err)
	}
	if !reflect.DeepEqual(revs, []string{"web-00002", "web-00001"}) {
		t.Errorf("weighted revision 00001 must survive a limit of 1: %v", revs)
	}
}

func TestRollout_MissingRevisionIsSkipped(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	mustApply(t, o, testRequest())
	// Head Revision vanished (e.g. mid-teardown): the sweep must
	// not cut or crash.
	if err := o.revisions.Delete(t.Context(), "orchestrator", "dep-web-00001", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete revision: %v", err)
	}

	reconcileAll(t, o)

	if m := getMarkerData(t, o, "web"); m.LastReady != "" {
		t.Errorf("no cut expected, lastReady=%s", m.LastReady)
	}
}
