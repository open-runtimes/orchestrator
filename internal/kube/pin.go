package kube

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

// Pin softly sends a workload's pods back to the same few nodes, so whatever
// a node kept from the last start (pulled images, cached artifacts) is there
// for the next one. It ranks the nodes a workload pod can land on by
// hash(key, node) and prefers the top n: rendezvous hashing, so a node joining
// or leaving moves only the keys that ranked it. The preference is soft. The
// scheduler still places the pod elsewhere when those nodes are full.
//
// A nil Pin pins nothing.
type Pin struct {
	n           int
	tolerations []corev1.Toleration
	nodes       listersv1.NodeLister
}

// NewPin watches the nodes matching selector and prefers n of them per key.
// n <= 0 returns nil: the pin is off (KUBE_WORKLOAD_PIN_NODES, default 0).
// Until the first list lands, Affinity returns nil, so a pod created at
// startup is simply unpinned.
func NewPin(client kubernetes.Interface, n int, selector map[string]string, tolerations []corev1.Toleration) *Pin {
	if n <= 0 {
		return nil
	}
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = labels.SelectorFromSet(selector).String()
		}))
	nodes := factory.Core().V1().Nodes().Lister()
	factory.Start(context.Background().Done()) // lives as long as the process
	return &Pin{n: n, tolerations: tolerations, nodes: nodes}
}

// Affinity is the preferred node affinity for key's top n nodes, or nil when
// the pin is off, key is empty or no node qualifies. Cordoned nodes and nodes
// with a NoSchedule or NoExecute taint the workload does not tolerate are
// left out, so a draining or compacting node is never preferred.
func (p *Pin) Affinity(key string) *corev1.Affinity {
	if p == nil || key == "" {
		return nil
	}
	nodes, _ := p.nodes.List(labels.Everything())
	type ranked struct {
		host  string
		score uint64
	}
	var candidates []ranked
	for _, node := range nodes {
		if !p.schedulable(node) {
			continue
		}
		host := node.Labels[corev1.LabelHostname]
		if host == "" {
			continue
		}
		sum := sha256.Sum256([]byte(key + "\x00" + node.Name))
		candidates = append(candidates, ranked{host, binary.BigEndian.Uint64(sum[:8])})
	}
	if len(candidates) == 0 {
		return nil
	}
	slices.SortFunc(candidates, func(a, b ranked) int { return cmp.Compare(b.score, a.score) })
	candidates = candidates[:min(p.n, len(candidates))]
	hosts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		hosts = append(hosts, c.host)
	}
	slices.Sort(hosts) // a stable spec for the same set
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
			Weight: 100,
			Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: hosts,
			}}},
		}},
	}}
}

func (p *Pin) schedulable(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect == corev1.TaintEffectPreferNoSchedule {
			continue
		}
		if !slices.ContainsFunc(p.tolerations, func(t corev1.Toleration) bool { return t.ToleratesTaint(klog.Background(), taint, true) }) {
			return false
		}
	}
	return true
}
