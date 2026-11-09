package kube

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func pinNode(name string, pool map[string]string, mutate ...func(*corev1.Node)) *corev1.Node {
	l := map[string]string{corev1.LabelHostname: name}
	maps.Copy(l, pool)
	n := &corev1.Node{Name: name, Labels: l}
	for _, m := range mutate {
		m(n)
	}
	return n
}

// preferred waits for the pin's node watch to land, then returns the hosts it
// prefers for key.
func preferred(t *testing.T, p *Pin, key string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if a := p.Affinity(key); a != nil {
			terms := a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution
			if len(terms) != 1 || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
				t.Fatalf("want one preferred term and nothing required, got %+v", a.NodeAffinity)
			}
			return terms[0].Preference.MatchExpressions[0].Values
		}
		if time.Now().After(deadline) {
			t.Fatalf("no affinity for %q", key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPin_PrefersTheSameNodesForAKeyAndOnlyEligibleOnes(t *testing.T) {
	pool := map[string]string{"workload": "edge"}
	objects := []runtime.Object{
		pinNode("cordoned", pool, func(n *corev1.Node) { n.Spec.Unschedulable = true }),
		pinNode("compacting", pool, func(n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "compaction", Value: "true", Effect: corev1.TaintEffectNoSchedule}}
		}),
		pinNode("other-pool", map[string]string{"workload": "builds"}),
	}
	for i := range 10 {
		objects = append(objects, pinNode(fmt.Sprintf("edge-%d", i), pool, func(n *corev1.Node) {
			// The pool's own taint is tolerated, so these stay eligible.
			n.Spec.Taints = []corev1.Taint{{Key: "workload", Value: "edge", Effect: corev1.TaintEffectNoSchedule}}
		}))
	}
	tolerations := []corev1.Toleration{{Key: "workload", Operator: corev1.TolerationOpEqual, Value: "edge", Effect: corev1.TaintEffectNoSchedule}}
	p := NewPin(fake.NewClientset(objects...), 3, pool, tolerations)

	seen := map[string]bool{}
	for i := range 50 {
		key := fmt.Sprintf("deployment-%d", i)
		hosts := preferred(t, p, key)
		if len(hosts) != 3 {
			t.Fatalf("%s: want 3 hosts, got %v", key, hosts)
		}
		if again := preferred(t, p, key); !slices.Equal(hosts, again) {
			t.Fatalf("%s: %v then %v", key, hosts, again)
		}
		for _, h := range hosts {
			if h == "cordoned" || h == "compacting" || h == "other-pool" {
				t.Fatalf("%s: preferred ineligible node %s", key, h)
			}
			seen[h] = true
		}
	}
	if len(seen) < 8 {
		t.Fatalf("50 keys landed on only %d of 10 nodes: %v", len(seen), seen)
	}
}

func TestPin_RemovingANodeMovesOnlyTheKeysThatRankedIt(t *testing.T) {
	var objects []runtime.Object
	for i := range 10 {
		objects = append(objects, pinNode(fmt.Sprintf("n-%d", i), nil))
	}
	client := fake.NewClientset(objects...)
	p := NewPin(client, 3, nil, nil)

	before := map[string][]string{}
	for i := range 50 {
		key := fmt.Sprintf("k-%d", i)
		before[key] = preferred(t, p, key)
	}
	if err := client.CoreV1().Nodes().Delete(t.Context(), "n-4", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		nodes, _ := p.nodes.List(labels.Everything())
		if len(nodes) == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node deletion never reached the pin")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for key, hosts := range before {
		after := preferred(t, p, key)
		if slices.Contains(after, "n-4") {
			t.Fatalf("%s still prefers the deleted node", key)
		}
		kept := 0
		for _, h := range after {
			if slices.Contains(hosts, h) {
				kept++
			}
		}
		want := 3
		if slices.Contains(hosts, "n-4") {
			want = 2
		}
		if kept != want {
			t.Fatalf("%s: %v became %v", key, hosts, after)
		}
	}
}

func TestPin_OffOrKeylessPinsNothing(t *testing.T) {
	if p := NewPin(fake.NewClientset(pinNode("n", nil)), 0, nil, nil); p != nil || p.Affinity("k") != nil {
		t.Fatal("pinNodes 0 must pin nothing")
	}
	p := NewPin(fake.NewClientset(pinNode("n", nil)), 3, nil, nil)
	preferred(t, p, "k")
	if p.Affinity("") != nil {
		t.Fatal("an empty key must pin nothing")
	}
}
