package endpointflip

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testNS            = "orch"
	testProxyPort     = int32(8000)
	testActivatorPort = int32(8081)
	activatorLabelKey = "app.kubernetes.io/component"
	activatorLabelVal = "deployments-activator"
)

func testOptions() Options {
	return Options{
		ActivatorSelector: activatorLabelKey + "=" + activatorLabelVal,
		ProxyPort:         testProxyPort,
		ActivatorPort:     testActivatorPort,
	}
}

func newTestReconciler(objs ...runtime.Object) (*Reconciler, *fake.Clientset) {
	client := fake.NewClientset(objs...)
	return New(client, testNS, testOptions()), client
}

func revisionService(rev, depID string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dep-" + rev,
			Namespace: testNS,
			UID:       types.UID("uid-" + rev),
			Labels: map[string]string{
				LabelManagedBy:    ManagedByValue,
				LabelDeploymentID: depID,
				LabelRevision:     rev,
			},
		},
	}
}

func podObject(name string, podLabels map[string]string, ip string, ready, terminating bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: podLabels},
		Status: corev1.PodStatus{
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
	if terminating {
		ts := metav1.Unix(0, 0)
		pod.DeletionTimestamp = &ts
	}
	return pod
}

func revisionPod(name, rev, ip string, ready bool) *corev1.Pod {
	return podObject(name, map[string]string{LabelRevision: rev}, ip, ready, false)
}

func activatorPod(name, ip string, ready bool) *corev1.Pod {
	return podObject(name, map[string]string{activatorLabelKey: activatorLabelVal}, ip, ready, false)
}

func mustReconcile(t *testing.T, r *Reconciler, serviceName string) {
	t.Helper()
	if err := r.reconcileService(t.Context(), serviceName); err != nil {
		t.Fatalf("reconcileService(%s): %v", serviceName, err)
	}
}

func getSlice(t *testing.T, client kubernetes.Interface, serviceName string) *discoveryv1.EndpointSlice {
	t.Helper()
	slice, err := client.DiscoveryV1().EndpointSlices(testNS).Get(t.Context(), serviceName+sliceSuffix, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get slice for %s: %v", serviceName, err)
	}
	return slice
}

func sliceIPs(slice *discoveryv1.EndpointSlice) []string {
	var ips []string
	for _, ep := range slice.Endpoints {
		ips = append(ips, ep.Addresses...)
	}
	slices.Sort(ips)
	return ips
}

func slicePort(t *testing.T, slice *discoveryv1.EndpointSlice) int32 {
	t.Helper()
	if len(slice.Ports) != 1 || slice.Ports[0].Port == nil {
		t.Fatalf("expected exactly one port with a value, got %+v", slice.Ports)
	}
	return *slice.Ports[0].Port
}

func assertSlice(t *testing.T, slice *discoveryv1.EndpointSlice, wantIPs []string, wantPort int32) {
	t.Helper()
	if got := sliceIPs(slice); !slices.Equal(got, wantIPs) {
		t.Errorf("endpoints = %v, want %v", got, wantIPs)
	}
	if got := slicePort(t, slice); got != wantPort {
		t.Errorf("port = %d, want %d", got, wantPort)
	}
	for _, ep := range slice.Endpoints {
		if ep.Conditions.Ready == nil || !*ep.Conditions.Ready {
			t.Errorf("endpoint %v not marked ready", ep.Addresses)
		}
	}
}

func TestWarmUsesReadyRevisionPods(t *testing.T) {
	r, client := newTestReconciler(
		revisionService("web-00001", "web"),
		revisionPod("web-a", "web-00001", "10.0.0.1", true),
		revisionPod("web-b", "web-00001", "10.0.0.2", true),
		revisionPod("web-c", "web-00001", "10.0.0.3", false),
		activatorPod("act-0", "10.9.0.0", true),
	)
	mustReconcile(t, r, "dep-web-00001")

	slice := getSlice(t, client, "dep-web-00001")
	assertSlice(t, slice, []string{"10.0.0.1", "10.0.0.2"}, testProxyPort)
	if got := *slice.Ports[0].Name; got != portName {
		t.Errorf("port name = %q, want %q", got, portName)
	}
}

func TestFlipDownToActivators(t *testing.T) {
	pod := revisionPod("web-a", "web-00001", "10.0.0.1", true)
	r, client := newTestReconciler(
		revisionService("web-00001", "web"),
		pod,
		activatorPod("act-0", "10.9.0.0", true),
		activatorPod("act-1", "10.9.0.1", true),
	)
	mustReconcile(t, r, "dep-web-00001")
	assertSlice(t, getSlice(t, client, "dep-web-00001"), []string{"10.0.0.1"}, testProxyPort)

	unready := revisionPod("web-a", "web-00001", "10.0.0.1", false)
	if _, err := client.CoreV1().Pods(testNS).Update(t.Context(), unready, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, "dep-web-00001")
	assertSlice(t, getSlice(t, client, "dep-web-00001"), []string{"10.9.0.0", "10.9.0.1"}, testActivatorPort)
}

func TestFlipUpToRevisionPods(t *testing.T) {
	r, client := newTestReconciler(
		revisionService("web-00001", "web"),
		revisionPod("web-a", "web-00001", "10.0.0.1", false),
		activatorPod("act-0", "10.9.0.0", true),
	)
	mustReconcile(t, r, "dep-web-00001")
	assertSlice(t, getSlice(t, client, "dep-web-00001"), []string{"10.9.0.0"}, testActivatorPort)

	ready := revisionPod("web-a", "web-00001", "10.0.0.1", true)
	if _, err := client.CoreV1().Pods(testNS).Update(t.Context(), ready, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, "dep-web-00001")
	assertSlice(t, getSlice(t, client, "dep-web-00001"), []string{"10.0.0.1"}, testProxyPort)
}

func TestColdWithNoActivatorsKeepsEmptySlice(t *testing.T) {
	r, client := newTestReconciler(
		revisionService("web-00001", "web"),
		revisionPod("web-a", "web-00001", "10.0.0.1", false),
		activatorPod("act-0", "10.9.0.0", false),
	)
	mustReconcile(t, r, "dep-web-00001")

	slice := getSlice(t, client, "dep-web-00001")
	if len(slice.Endpoints) != 0 {
		t.Errorf("expected zero endpoints, got %v", sliceIPs(slice))
	}
	if got := slicePort(t, slice); got != testActivatorPort {
		t.Errorf("port = %d, want activator port %d", got, testActivatorPort)
	}
	if slice.AddressType != discoveryv1.AddressTypeIPv4 {
		t.Errorf("addressType = %s, want IPv4", slice.AddressType)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	cases := map[string][]runtime.Object{
		"warm": {
			revisionService("web-00001", "web"),
			revisionPod("web-a", "web-00001", "10.0.0.1", true),
			activatorPod("act-0", "10.9.0.0", true),
		},
		"cold": {
			revisionService("web-00001", "web"),
			activatorPod("act-0", "10.9.0.0", true),
		},
		"cold-empty": {
			revisionService("web-00001", "web"),
		},
	}
	for name, objs := range cases {
		t.Run(name, func(t *testing.T) {
			r, client := newTestReconciler(objs...)
			mustReconcile(t, r, "dep-web-00001")

			client.ClearActions()
			mustReconcile(t, r, "dep-web-00001")
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "endpointslices" {
					continue
				}
				if verb := action.GetVerb(); verb == "create" || verb == "update" {
					t.Errorf("unexpected %s on endpointslices during no-change reconcile", verb)
				}
			}
		})
	}
}

func TestSliceOwnershipAndDiscoveryLabels(t *testing.T) {
	svc := revisionService("web-00001", "web")
	r, client := newTestReconciler(svc, revisionPod("web-a", "web-00001", "10.0.0.1", true))
	mustReconcile(t, r, "dep-web-00001")

	slice := getSlice(t, client, "dep-web-00001")
	wantLabels := map[string]string{
		discoveryv1.LabelServiceName: "dep-web-00001",
		discoveryv1.LabelManagedBy:   ManagedByValue,
		LabelManagedBy:               ManagedByValue,
		LabelDeploymentID:            "web",
		LabelRevision:                "web-00001",
	}
	for k, want := range wantLabels {
		if got := slice.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	if len(slice.OwnerReferences) != 1 {
		t.Fatalf("ownerReferences = %+v, want exactly one", slice.OwnerReferences)
	}
	owner := slice.OwnerReferences[0]
	if owner.Kind != "Service" || owner.APIVersion != "v1" || owner.Name != svc.Name || owner.UID != svc.UID {
		t.Errorf("ownerReference = %+v, want Service %s (uid %s)", owner, svc.Name, svc.UID)
	}
	if owner.Controller == nil || !*owner.Controller {
		t.Error("ownerReference should be marked controller")
	}
}

func TestSkipsUnmanagedAndMissingServices(t *testing.T) {
	plain := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "plain",
			Namespace: testNS,
			Labels:    map[string]string{LabelManagedBy: ManagedByValue, LabelDeploymentID: "web"},
		},
	}
	r, client := newTestReconciler(plain)
	mustReconcile(t, r, "plain")   // managed, but no revision label
	mustReconcile(t, r, "missing") // service does not exist

	list, err := client.DiscoveryV1().EndpointSlices(testNS).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("expected no slices, got %d", len(list.Items))
	}
}

func TestRunLoopFlipsOnPodEvents(t *testing.T) {
	r, client := newTestReconciler(
		revisionService("web-00001", "web"),
		activatorPod("act-0", "10.9.0.0", true),
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Service add event: cold, so the activator backs it.
	waitForSlice(t, client, "dep-web-00001", []string{"10.9.0.0"}, testActivatorPort)

	pod := revisionPod("web-a", "web-00001", "10.0.0.5", true)
	if _, err := client.CoreV1().Pods(testNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForSlice(t, client, "dep-web-00001", []string{"10.0.0.5"}, testProxyPort)

	if err := client.CoreV1().Pods(testNS).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForSlice(t, client, "dep-web-00001", []string{"10.9.0.0"}, testActivatorPort)
}

func waitForSlice(t *testing.T, client kubernetes.Interface, serviceName string, wantIPs []string, wantPort int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		slice, err := client.DiscoveryV1().EndpointSlices(testNS).Get(t.Context(), serviceName+sliceSuffix, metav1.GetOptions{})
		if err == nil && slices.Equal(sliceIPs(slice), wantIPs) && len(slice.Ports) == 1 &&
			slice.Ports[0].Port != nil && *slice.Ports[0].Port == wantPort {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slice for %s never reached %v on port %d (last: slice=%+v err=%v)", serviceName, wantIPs, wantPort, slice, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
