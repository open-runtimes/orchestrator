package kubernetes

import (
	"context"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/workload"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// servicePort is the stable port every activation Service exposes.
const servicePort int32 = 80

// activationObjectName names an activation's Service and HTTPRoute.
func activationObjectName(activationID string) string {
	return "act-" + activationID
}

// activationHost resolves an activation's public hostname: the caller-chosen
// host, else {id}.{pool-domain}.
func activationHost(host, activationID, domain string) string {
	if host != "" {
		return host
	}
	return activationID + "." + domain
}

// published records which of an activation's routing objects THIS request
// created. An object that was already there was published by another request
// carrying the same activation id — the duplicate-id guard is a read taken
// before the claim, so two concurrent creates can both get past it — and
// removing it would cut that activation's URL out from under it. So cleanup
// removes only what it made.
type published struct {
	service bool
	route   bool
}

// createActivationService creates the activation's ClusterIP Service: port
// 80 → the claimed pod's proxy data port, selected by the activation label.
// Reports false when the Service was already there, so a failed create does not
// tear down another request's.
func (o *Orchestrator) createActivationService(ctx context.Context, poolID, activationID string) (bool, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:   activationObjectName(activationID),
			Labels: activationLabels(poolID, activationID),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{LabelActivation: activationID},
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       servicePort,
				TargetPort: intstr.FromInt32(workload.DefaultProxyPort),
			}},
		},
	}
	switch _, err := o.client.CoreV1().Services(o.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{}); {
	case err == nil:
		return true, nil
	case apierrors.IsAlreadyExists(err):
		return false, nil
	default:
		return false, apperrors.Internal("kubernetes.createService", err)
	}
}

// createActivationRoute creates the activation's HTTPRoute: its hostname,
// the operator's Gateway as parentRef, and a single rule backending the
// activation Service — the deployments route shape minus traffic splitting
// and async rules, which have no meaning for a single bound pod. Reports false
// when the route was already there, or when there is no gateway to publish to.
func (o *Orchestrator) createActivationRoute(ctx context.Context, poolID, activationID, host string) (bool, error) {
	if !o.cfg.GatewayEnabled {
		return false, nil
	}
	route := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:   activationObjectName(activationID),
			Labels: activationLabels(poolID, activationID),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Name:      gatewayv1.ObjectName(o.cfg.GatewayName),
					Namespace: ptr.To(gatewayv1.Namespace(o.cfg.GatewayNamespace)),
				}},
			},
			Hostnames: []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Name: gatewayv1.ObjectName(activationObjectName(activationID)),
							Port: ptr.To(servicePort),
						},
					},
				}},
			}},
		},
	}
	switch _, err := o.gateway.GatewayV1().HTTPRoutes(o.cfg.Namespace).Create(ctx, route, metav1.CreateOptions{}); {
	case err == nil:
		return true, nil
	case apierrors.IsAlreadyExists(err):
		return false, nil
	default:
		return false, apperrors.Internal("kubernetes.createRoute", err)
	}
}

// deleteActivationObjects removes the activation's HTTPRoute and Service,
// tolerating already-gone objects (exec activations never had them). what bounds
// it to the objects the caller is entitled to remove.
func (o *Orchestrator) deleteActivationObjects(ctx context.Context, activationID string, what published) error {
	if what.route && o.cfg.GatewayEnabled {
		err := o.gateway.GatewayV1().HTTPRoutes(o.cfg.Namespace).Delete(ctx, activationObjectName(activationID), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return apperrors.Internal("kubernetes.deleteRoute", err)
		}
	}
	if !what.service {
		return nil
	}
	err := o.client.CoreV1().Services(o.cfg.Namespace).Delete(ctx, activationObjectName(activationID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteService", err)
	}
	return nil
}

// activationLabels are stamped on an activation's Service and HTTPRoute (the
// claimed pod gains LabelActivation by patch instead).
func activationLabels(poolID, activationID string) map[string]string {
	return map[string]string{
		LabelManagedBy:  ManagedByValue,
		LabelPoolID:     poolID,
		LabelActivation: activationID,
	}
}
