package kubernetes

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"orchestrator/internal/job"
	"orchestrator/internal/kube"
)

func TestRun_PlacementKeyPrefersThePinnedNodes(t *testing.T) {
	t.Parallel()
	o, cs := newTestOrchestrator(t, noopWatcher{})
	defer o.Close()
	for _, n := range []string{"a", "b", "c", "d"} {
		if _, err := cs.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
			Name: n, Labels: map[string]string{corev1.LabelHostname: n}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	o.pin = kube.NewPin(cs, 3, nil, nil)
	for deadline := time.Now().Add(5 * time.Second); o.pin.Affinity("fn-1") == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("pin never saw the nodes")
		}
	}

	for _, req := range []*job.Request{
		{ID: "keyed", Image: "alpine", TimeoutSeconds: 60, PlacementKey: "fn-1"},
		{ID: "unkeyed", Image: "alpine", TimeoutSeconds: 60},
	} {
		if err := o.Run(t.Context(), req); err != nil {
			t.Fatalf("Run %s: %v", req.ID, err)
		}
	}
	keyed, err := cs.BatchV1().Jobs("orchestrator").Get(t.Context(), "job-keyed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	terms := keyed.Spec.Template.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(terms) != 1 || len(terms[0].Preference.MatchExpressions[0].Values) != 3 {
		t.Fatalf("keyed job affinity = %+v", keyed.Spec.Template.Spec.Affinity)
	}
	unkeyed, err := cs.BatchV1().Jobs("orchestrator").Get(t.Context(), "job-unkeyed", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if unkeyed.Spec.Template.Spec.Affinity != nil {
		t.Fatalf("a job with no placement key must not be pinned: %+v", unkeyed.Spec.Template.Spec.Affinity)
	}
}
