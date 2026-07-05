package kubernetes

import (
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"reflect"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func canarySplit() []deployment.Target {
	return []deployment.Target{
		{RevisionName: "web-00002", Percent: 90},
		{RevisionName: "web-00001", Percent: 10},
	}
}

func TestSetTraffic_WritesBothRules(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)

	if err := o.SetTraffic(t.Context(), "web", canarySplit()); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}

	route := getRoute(t, o, "web")
	if len(route.Spec.Rules) != 2 {
		t.Fatalf("rules: want 2 (async + default), got %d", len(route.Spec.Rules))
	}
	assertAsyncRule(t, route.Spec.Rules[0], o.cfg, canarySplit())
	assertDefaultRule(t, route.Spec.Rules[1], canarySplit())

	if len(route.Spec.ParentRefs) != 1 || string(route.Spec.ParentRefs[0].Name) != "orchestrator" {
		t.Errorf("parentRefs: got %+v", route.Spec.ParentRefs)
	}
}

// assertAsyncRule checks the Prefer: respond-async rule: exact header match,
// every weighted backendRef pointing at the activator, each tagged with its
// revision.
func assertAsyncRule(t *testing.T, rule gatewayv1.HTTPRouteRule, cfg Config, targets []deployment.Target) {
	t.Helper()
	if len(rule.Matches) != 1 || len(rule.Matches[0].Headers) != 1 {
		t.Fatalf("async rule matches: got %+v", rule.Matches)
	}
	h := rule.Matches[0].Headers[0]
	if h.Type == nil || *h.Type != gatewayv1.HeaderMatchExact || string(h.Name) != "Prefer" || h.Value != "respond-async" {
		t.Errorf("async header match: want exact Prefer=respond-async, got %+v", h)
	}
	if len(rule.BackendRefs) != len(targets) {
		t.Fatalf("async backendRefs: want %d, got %d", len(targets), len(rule.BackendRefs))
	}
	for i, ref := range rule.BackendRefs {
		if string(ref.Name) != cfg.ActivatorService {
			t.Errorf("async ref %d: want activator %q, got %q", i, cfg.ActivatorService, ref.Name)
		}
		if ref.Port == nil || int(*ref.Port) != cfg.ActivatorPort {
			t.Errorf("async ref %d port: want %d, got %v", i, cfg.ActivatorPort, ref.Port)
		}
		assertBackendRefShape(t, ref, targets[i])
	}
}

// assertDefaultRule checks the match-less rule: weighted backendRefs across
// the revision Services on port 80.
func assertDefaultRule(t *testing.T, rule gatewayv1.HTTPRouteRule, targets []deployment.Target) {
	t.Helper()
	if len(rule.Matches) != 0 {
		t.Fatalf("default rule must have no matches, got %+v", rule.Matches)
	}
	if len(rule.BackendRefs) != len(targets) {
		t.Fatalf("default backendRefs: want %d, got %d", len(targets), len(rule.BackendRefs))
	}
	for i, ref := range rule.BackendRefs {
		if string(ref.Name) != objectNameFor(targets[i].RevisionName) {
			t.Errorf("default ref %d: want %s, got %s", i, objectNameFor(targets[i].RevisionName), ref.Name)
		}
		if ref.Port == nil || *ref.Port != 80 {
			t.Errorf("default ref %d port: want 80, got %v", i, ref.Port)
		}
		assertBackendRefShape(t, ref, targets[i])
	}
}

// assertBackendRefShape checks weight and the per-backendRef X-Revision SET
// filter (set, never add — a client-supplied X-Revision is overwritten).
func assertBackendRefShape(t *testing.T, ref gatewayv1.HTTPBackendRef, target deployment.Target) {
	t.Helper()
	if ref.Weight == nil || int(*ref.Weight) != target.Percent {
		t.Errorf("ref %s weight: want %d, got %v", target.RevisionName, target.Percent, ref.Weight)
	}
	if len(ref.Filters) != 1 {
		t.Fatalf("ref %s: want exactly 1 filter, got %d", target.RevisionName, len(ref.Filters))
	}
	f := ref.Filters[0]
	if f.Type != gatewayv1.HTTPRouteFilterRequestHeaderModifier || f.RequestHeaderModifier == nil {
		t.Fatalf("ref %s filter: want RequestHeaderModifier, got %+v", target.RevisionName, f)
	}
	if len(f.RequestHeaderModifier.Add) != 0 {
		t.Errorf("ref %s: X-Revision must be SET, never ADD", target.RevisionName)
	}
	set := f.RequestHeaderModifier.Set
	if len(set) != 1 || string(set[0].Name) != "X-Revision" || set[0].Value != target.RevisionName {
		t.Errorf("ref %s: want set X-Revision=%s, got %+v", target.RevisionName, target.RevisionName, set)
	}
}

func TestSetTraffic_ModeTransitions(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)

	// Any table other than 100% latest pins manual.
	if err := o.SetTraffic(t.Context(), "web", canarySplit()); err != nil {
		t.Fatalf("SetTraffic split: %v", err)
	}
	if m := getMarkerData(t, o, "web"); m.TrafficMode != trafficModeManual {
		t.Errorf("split: want manual mode, got %s", m.TrafficMode)
	}

	// 100% on an OLD revision (rollback pin) is still manual.
	if err := o.SetTraffic(t.Context(), "web", []deployment.Target{{RevisionName: "web-00001", Percent: 100}}); err != nil {
		t.Fatalf("SetTraffic rollback: %v", err)
	}
	if m := getMarkerData(t, o, "web"); m.TrafficMode != trafficModeManual {
		t.Errorf("rollback pin: want manual mode, got %s", m.TrafficMode)
	}

	// Exactly [{latest, 100}] re-arms the auto-cut.
	if err := o.SetTraffic(t.Context(), "web", []deployment.Target{{RevisionName: "web-00002", Percent: 100}}); err != nil {
		t.Fatalf("SetTraffic reset: %v", err)
	}
	if m := getMarkerData(t, o, "web"); m.TrafficMode != trafficModeAuto {
		t.Errorf("reset to latest: want auto mode, got %s", m.TrafficMode)
	}
}

func TestSetTraffic_UnknownRevisionIsValidationError(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	mustApply(t, o, testRequest())

	err := o.SetTraffic(t.Context(), "web", []deployment.Target{{RevisionName: "web-00042", Percent: 100}})
	if !errors.Is(err, apperrors.ErrValidation) {
		t.Errorf("want Validation error for a missing revision, got %v", err)
	}
}

func TestSetTraffic_NotFound(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	err := o.SetTraffic(t.Context(), "ghost", []deployment.Target{{RevisionName: "ghost-00001", Percent: 100}})
	if !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("want NotFound, got %v", err)
	}
}

func TestRouteTargets_RoundTrip(t *testing.T) {
	t.Parallel()
	o, _ := newTestOrchestrator(t)
	twoRevisions(t, o)

	want := canarySplit()
	if err := o.SetTraffic(t.Context(), "web", want); err != nil {
		t.Fatalf("SetTraffic: %v", err)
	}
	got := routeTargets(getRoute(t, o, "web"))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets round-trip: want %+v, got %+v", want, got)
	}
}

// TestRouteTargets_SurvivesAPIServerDefaulting parses a route the way a real
// API server returns it: CRD defaulting stamps a PathPrefix match onto every
// rule, so the default rule must be identified by NOT carrying the Prefer
// header match — not by being match-less.
func TestRouteTargets_SurvivesAPIServerDefaulting(t *testing.T) {
	t.Parallel()
	targets := []deployment.Target{
		{RevisionName: "web-00002", Percent: 90},
		{RevisionName: "web-00001", Percent: 10},
	}
	o := &Orchestrator{cfg: Config{GatewayName: "orchestrator", ActivatorService: "deployments-activator", ActivatorPort: 8081}}
	route := o.buildHTTPRoute(marker{ID: "web", Host: "web.example.com"}, targets)

	// Simulate API-server defaulting: every rule gains a PathPrefix / match.
	prefix := gatewayv1.PathMatchPathPrefix
	slash := "/"
	for i := range route.Spec.Rules {
		if len(route.Spec.Rules[i].Matches) == 0 {
			route.Spec.Rules[i].Matches = []gatewayv1.HTTPRouteMatch{{}}
		}
		for j := range route.Spec.Rules[i].Matches {
			route.Spec.Rules[i].Matches[j].Path = &gatewayv1.HTTPPathMatch{Type: &prefix, Value: &slash}
		}
	}

	got := routeTargets(route)
	if len(got) != 2 || got[0] != targets[0] || got[1] != targets[1] {
		t.Fatalf("routeTargets after defaulting: got %+v, want %+v", got, targets)
	}
}
