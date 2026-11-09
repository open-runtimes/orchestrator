package kubernetes

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"orchestrator/internal/kube"
)

// Every revision of a deployment prefers the same nodes, so a new build lands
// where the last one's images and artifacts still are.
func TestRevisionPods_PreferTheDeploymentsPinnedNodes(t *testing.T) {
	o, cs := newTestOrchestrator(t)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if _, err := cs.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
			Name: n, Labels: map[string]string{corev1.LabelHostname: n}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	o.pin = kube.NewPin(cs, 3, nil, nil)
	for deadline := time.Now().Add(5 * time.Second); o.pin.Affinity("x") == nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("pin never saw the nodes")
		}
	}

	req := testRequest()
	req.Replicas = 1
	mustApply(t, o, req)
	req = testRequest()
	req.Replicas = 1
	req.Image = "nginx:next"
	mustApply(t, o, req)

	pods, err := cs.CoreV1().Pods(o.namespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelDeploymentID + "=" + req.ID})
	if err != nil {
		t.Fatal(err)
	}
	revisions := map[string]bool{}
	var hosts [][]string
	for _, p := range pods.Items {
		revisions[p.Labels[LabelRevision]] = true
		if p.Spec.Affinity == nil {
			t.Fatalf("pod %s is not pinned", p.Name)
		}
		hosts = append(hosts, p.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution[0].Preference.MatchExpressions[0].Values)
	}
	if len(revisions) != 2 {
		t.Fatalf("want pods of two revisions, got %v", revisions)
	}
	for _, h := range hosts[1:] {
		if !slices.Equal(h, hosts[0]) || len(h) != 3 {
			t.Fatalf("revisions prefer different nodes: %v", hosts)
		}
	}
	if revision, err := o.revisions.Get(t.Context(), o.namespace, objectNameFor("web-00001")); err != nil || revision.Spec.Template.Spec.Affinity != nil {
		t.Fatalf("the pin must stay out of the revision template (%v): %+v", err, revision)
	}
}
