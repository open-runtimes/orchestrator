package kubernetes

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"orchestrator/internal/kube"
	"orchestrator/internal/sandbox"
	"orchestrator/internal/warm"
)

func TestCreate_PoollessSandboxPrefersItsPlacementKeysNodes(t *testing.T) {
	t.Parallel()
	o, cs, _ := newTestOrchestrator(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		if _, err := cs.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
			Name: n, Labels: map[string]string{corev1.LabelHostname: n}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pin := kube.NewPin(cs, 3, nil, nil)
	for deadline := time.Now().Add(5 * time.Second); pin.Affinity("x") == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("pin never saw the nodes")
		}
	}
	cfg := o.cfg
	o = wireOrchestrator(cs, cfg, func(w *warm.Config) {
		w.Client = newFakeSidecar()
		w.Poll = time.Millisecond
		w.ColdWait = time.Second
		w.ServeWait = 50 * time.Millisecond
		w.Pin = pin
	})
	readyOnCreate(cs, "10.0.0.9")

	if _, err := o.Create(t.Context(), &sandbox.Request{
		ID: "solo", Token: "9f3c1a04b7e28d65f1024c8ba3e7d95f", TimeoutSeconds: new(300),
		Image: "python:3.12-slim", Port: 8000, PlacementKey: "project-1",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pods, err := cs.CoreV1().Pods(testNS).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods = %d (%v)", len(pods.Items), err)
	}
	got := pods.Items[0].Spec.Affinity
	if got == nil || len(got.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Preference.MatchExpressions[0].Values) != 3 {
		t.Fatalf("pod affinity = %+v", got)
	}
}
