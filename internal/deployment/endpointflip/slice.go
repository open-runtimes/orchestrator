package endpointflip

import (
	"context"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
)

// reconcileService converges the {service}-flip EndpointSlice for one managed
// revision Service. A missing or unmanaged Service is a no-op: the slice is
// owned by the Service, so deletion cascades.
func (r *Reconciler) reconcileService(ctx context.Context, name string) error {
	svc, err := r.client.CoreV1().Services(r.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if svc.Labels[LabelManagedBy] != ManagedByValue || svc.Labels[LabelRevision] == "" {
		return nil
	}

	ips, port, err := r.desiredMembership(ctx, svc.Labels[LabelRevision])
	if err != nil {
		return err
	}
	return r.apply(ctx, desiredSlice(svc, ips, port))
}

// desiredMembership decides the flip by ready-pod count, not autoscaler
// intent: any ready revision pod → warm (pod IPs on ProxyPort); otherwise
// activator IPs on ActivatorPort — possibly zero of them, never stale pod IPs.
func (r *Reconciler) desiredMembership(ctx context.Context, revision string) ([]string, int32, error) {
	ips, err := r.readyPodIPs(ctx, r.namespace, labels.Set{LabelRevision: revision}.AsSelector())
	if err != nil {
		return nil, 0, err
	}
	if len(ips) > 0 {
		return ips, r.opts.ProxyPort, nil
	}
	if r.activatorSelector == nil {
		return nil, r.opts.ActivatorPort, nil
	}
	ips, err = r.readyPodIPs(ctx, r.activatorNamespace, r.activatorSelector)
	if err != nil {
		return nil, 0, err
	}
	return ips, r.opts.ActivatorPort, nil
}

func (r *Reconciler) readyPodIPs(ctx context.Context, namespace string, sel labels.Selector) ([]string, error) {
	list, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	var ips []string
	for i := range list.Items {
		if isReady(&list.Items[i]) {
			ips = append(ips, list.Items[i].Status.PodIP)
		}
	}
	slices.Sort(ips)
	return slices.Compact(ips), nil
}

// isReady gates endpoint membership: a terminating (draining) pod is not a
// warm endpoint even while its Ready condition still reads true.
func isReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" {
		return false
	}
	if pod.Labels[LabelPoolClaim] != "" && pod.Labels[LabelServing] != "true" {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// desiredSlice builds the flip slice: the kubernetes.io/service-name label is
// how consumers resolve it, and the ownerReference makes deletion cascade
// from the Service.
func desiredSlice(svc *corev1.Service, ips []string, port int32) *discoveryv1.EndpointSlice {
	endpoints := make([]discoveryv1.Endpoint, 0, len(ips))
	for _, ip := range ips {
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses:  []string{ip},
			Conditions: discoveryv1.EndpointConditions{Ready: ptrTo(true)},
		})
	}
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svc.Name + sliceSuffix,
			Namespace: svc.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: svc.Name,
				discoveryv1.LabelManagedBy:   ManagedByValue,
				LabelManagedBy:               ManagedByValue,
				LabelDeploymentID:            svc.Labels[LabelDeploymentID],
				LabelRevision:                svc.Labels[LabelRevision],
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Service",
				Name:       svc.Name,
				UID:        svc.UID,
				Controller: ptrTo(true),
			}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptrTo(portName),
			Port:     ptrTo(port),
			Protocol: ptrTo(corev1.ProtocolTCP),
		}},
	}
}

// apply creates or updates the slice, skipping the write when endpoint and
// port membership already match.
func (r *Reconciler) apply(ctx context.Context, desired *discoveryv1.EndpointSlice) error {
	api := r.client.DiscoveryV1().EndpointSlices(desired.Namespace)
	current, err := api.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if membershipEqual(current, desired) {
		return nil
	}
	desired.ResourceVersion = current.ResourceVersion
	_, err = api.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

// membershipEqual is order-insensitive over endpoints and ports.
func membershipEqual(a, b *discoveryv1.EndpointSlice) bool {
	return endpointSet(a).Equal(endpointSet(b)) && portSet(a).Equal(portSet(b))
}

func endpointSet(slice *discoveryv1.EndpointSlice) sets.Set[string] {
	keys := sets.New[string]()
	for _, ep := range slice.Endpoints {
		ready := ep.Conditions.Ready != nil && *ep.Conditions.Ready
		keys.Insert(strings.Join(ep.Addresses, ",") + "|" + strconv.FormatBool(ready))
	}
	return keys
}

func portSet(slice *discoveryv1.EndpointSlice) sets.Set[string] {
	keys := sets.New[string]()
	for _, p := range slice.Ports {
		var key strings.Builder
		if p.Name != nil {
			key.WriteString(*p.Name)
		}
		key.WriteByte('|')
		if p.Port != nil {
			key.WriteString(strconv.FormatInt(int64(*p.Port), 10))
		}
		key.WriteByte('|')
		if p.Protocol != nil {
			key.WriteString(string(*p.Protocol))
		}
		keys.Insert(key.String())
	}
	return keys
}

func ptrTo[T any](v T) *T { return &v }
