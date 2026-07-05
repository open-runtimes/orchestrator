package kubernetes

import (
	"context"
	"fmt"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	headerPrefer       = "Prefer"
	preferRespondAsync = "respond-async"
	headerRevision     = "X-Revision"

	// servicePort is the stable port every revision Service exposes.
	servicePort int32 = 80
)

// SetTraffic replaces the deployment's traffic table — canary, blue-green,
// or rollback are all weight edits across existing revisions. Every target
// revision must still exist. Pinning any table other than exactly
// [{latestRevision, 100}] switches the rollout mode to manual (no auto-cut);
// resetting to 100% latest re-arms it.
func (o *Orchestrator) SetTraffic(ctx context.Context, id string, targets []deployment.Target) error {
	m, err := o.getMarker(ctx, id)
	if err != nil {
		return err
	}
	for _, t := range targets {
		_, err := o.client.AppsV1().Deployments(o.namespace).Get(ctx, objectNameFor(t.RevisionName), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return apperrors.Validation("traffic.revisionName", fmt.Sprintf("revision %q does not exist", t.RevisionName))
		}
		if err != nil {
			return apperrors.Internal("kubernetes.getRevision", err)
		}
	}

	if err := o.writeRouteTraffic(ctx, m, targets); err != nil {
		return err
	}
	mode := trafficModeManual
	if len(targets) == 1 && targets[0].RevisionName == m.LatestRevision && targets[0].Percent == 100 {
		mode = trafficModeAuto
	}
	return o.updateMarker(ctx, id, func(m *marker) { m.TrafficMode = mode })
}

// currentTargets reads the traffic table back from the route's default rule.
// Without a route (gateway disabled, or a heal window) it falls back to a
// single 100% target on the last-ready revision (else the latest).
func (o *Orchestrator) currentTargets(ctx context.Context, m marker) []deployment.Target {
	if o.cfg.GatewayEnabled {
		route, err := o.gateway.GatewayV1().HTTPRoutes(o.namespace).Get(ctx, objectNameFor(m.ID), metav1.GetOptions{})
		if err == nil {
			if targets := routeTargets(route); len(targets) > 0 {
				return targets
			}
		}
	}
	return fallbackTargets(m)
}

// fallbackTargets is the traffic table implied by the marker alone.
func fallbackTargets(m marker) []deployment.Target {
	rev := m.LastReady
	if rev == "" {
		rev = m.LatestRevision
	}
	if rev == "" {
		return nil
	}
	return []deployment.Target{{RevisionName: rev, Percent: 100}}
}

// routeTargets parses the weighted revision split back out of the route's
// default (match-less) rule.
func routeTargets(route *gatewayv1.HTTPRoute) []deployment.Target {
	for _, rule := range route.Spec.Rules {
		if len(rule.Matches) > 0 {
			continue
		}
		targets := make([]deployment.Target, 0, len(rule.BackendRefs))
		for _, ref := range rule.BackendRefs {
			t := deployment.Target{RevisionName: revisionFromFilters(ref.Filters)}
			if ref.Weight != nil {
				t.Percent = int(*ref.Weight)
			}
			targets = append(targets, t)
		}
		return targets
	}
	return nil
}

// revisionFromFilters extracts the X-Revision set-header value stamped on a
// backendRef.
func revisionFromFilters(filters []gatewayv1.HTTPRouteFilter) string {
	for _, f := range filters {
		if f.RequestHeaderModifier == nil {
			continue
		}
		for _, h := range f.RequestHeaderModifier.Set {
			if string(h.Name) == headerRevision {
				return h.Value
			}
		}
	}
	return ""
}

// ensureRoute creates the deployment's HTTPRoute if missing (with the given
// traffic table) and keeps its hostname in sync with the spec. Existing
// traffic rules are never touched here — that is SetTraffic / the auto-cut.
func (o *Orchestrator) ensureRoute(ctx context.Context, m marker, targets []deployment.Target) error {
	if !o.cfg.GatewayEnabled {
		return nil
	}
	routes := o.gateway.GatewayV1().HTTPRoutes(o.namespace)
	existing, err := routes.Get(ctx, objectNameFor(m.ID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := routes.Create(ctx, o.buildHTTPRoute(m, targets), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return apperrors.Internal("kubernetes.createRoute", err)
		}
		return nil
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getRoute", err)
	}
	hostnames := []gatewayv1.Hostname{gatewayv1.Hostname(m.Host)}
	if len(existing.Spec.Hostnames) == 1 && existing.Spec.Hostnames[0] == hostnames[0] {
		return nil
	}
	existing.Spec.Hostnames = hostnames
	if _, err := routes.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.updateRoute", err)
	}
	return nil
}

// writeRouteTraffic rewrites both route rules for the given traffic table,
// creating the route if it is missing.
func (o *Orchestrator) writeRouteTraffic(ctx context.Context, m marker, targets []deployment.Target) error {
	if !o.cfg.GatewayEnabled {
		return nil
	}
	routes := o.gateway.GatewayV1().HTTPRoutes(o.namespace)
	existing, err := routes.Get(ctx, objectNameFor(m.ID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := routes.Create(ctx, o.buildHTTPRoute(m, targets), metav1.CreateOptions{}); err != nil {
			return apperrors.Internal("kubernetes.createRoute", err)
		}
		return nil
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getRoute", err)
	}
	existing.Spec.Rules = o.buildRouteRules(targets)
	if _, err := routes.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.updateRoute", err)
	}
	return nil
}

func (o *Orchestrator) deleteRoute(ctx context.Context, id string) error {
	if !o.cfg.GatewayEnabled {
		return nil
	}
	err := o.gateway.GatewayV1().HTTPRoutes(o.namespace).Delete(ctx, objectNameFor(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteRoute", err)
	}
	return nil
}

// buildHTTPRoute renders the deployment's HTTPRoute: hostname from the spec,
// parentRef to the operator's Gateway, and the two traffic rules.
func (o *Orchestrator) buildHTTPRoute(m marker, targets []deployment.Target) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name: objectNameFor(m.ID),
			Labels: map[string]string{
				LabelManagedBy:    ManagedByValue,
				LabelDeploymentID: m.ID,
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      gatewayv1.ObjectName(o.cfg.GatewayName),
					Namespace: ptr.To(gatewayv1.Namespace(o.cfg.GatewayNamespace)),
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(m.Host)},
			Rules:     o.buildRouteRules(targets),
		},
	}
}

// buildRouteRules compiles a traffic table into the route's two rules.
//
//  1. Async FIRST — an exact `Prefer: respond-async` header match carrying
//     the same weighted split, but every backendRef resolves to the shared
//     activator so async is buffered off-path. Each ref still carries its
//     revision tag, so the activator forwards for the revision the gateway
//     picked — a canary's cold 10% never lands on the wrong revision.
//  2. Default — weighted backendRefs across the revision Services.
//
// Every backendRef SETS (never adds) X-Revision at the edge, overwriting any
// client-supplied value. Per-backendRef header modification is Gateway API
// Extended — the design's one non-Core dependency.
func (o *Orchestrator) buildRouteRules(targets []deployment.Target) []gatewayv1.HTTPRouteRule {
	async := make([]gatewayv1.HTTPBackendRef, 0, len(targets))
	dflt := make([]gatewayv1.HTTPBackendRef, 0, len(targets))
	for _, t := range targets {
		async = append(async, backendRef(o.cfg.ActivatorService, int32(o.cfg.ActivatorPort), t))
		dflt = append(dflt, backendRef(objectNameFor(t.RevisionName), servicePort, t))
	}
	return []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Headers: []gatewayv1.HTTPHeaderMatch{{
					Type:  ptr.To(gatewayv1.HeaderMatchExact),
					Name:  headerPrefer,
					Value: preferRespondAsync,
				}},
			}},
			BackendRefs: async,
		},
		{BackendRefs: dflt},
	}
}

// backendRef renders one weighted backendRef tagged with its revision.
func backendRef(service string, port int32, t deployment.Target) gatewayv1.HTTPBackendRef {
	return gatewayv1.HTTPBackendRef{
		BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(service),
				Port: ptr.To(port),
			},
			Weight: ptr.To(int32(t.Percent)),
		},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
			RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set: []gatewayv1.HTTPHeader{{Name: headerRevision, Value: t.RevisionName}},
			},
		}},
	}
}
