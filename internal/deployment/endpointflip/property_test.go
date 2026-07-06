package endpointflip

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The invariant under test (docs/deployments.md): after every
// reconcile, slice endpoints are exactly the ready revision pods on ProxyPort
// when any exist, else exactly the ready activator pods on ActivatorPort — so
// the slice is never empty while at least one ready activator pod exists.

var propRevisions = []string{"web-00001", "web-00002"}

// propPod is the model's view of one pod; revision == "" marks an activator.
type propPod struct {
	name        string
	ip          string
	revision    string
	exists      bool
	ready       bool
	terminating bool
}

// memberReady mirrors the reconciler's endpoint-membership predicate.
func (p *propPod) memberReady() bool {
	return p.exists && p.ready && !p.terminating
}

func (p *propPod) toK8s() *corev1.Pod {
	podLabels := map[string]string{activatorLabelKey: activatorLabelVal}
	if p.revision != "" {
		podLabels = map[string]string{LabelRevision: p.revision}
	}
	return podObject(p.name, podLabels, p.ip, p.ready, p.terminating)
}

func newUniverse() []*propPod {
	var pods []*propPod
	for r, rev := range propRevisions {
		for i := range 3 {
			pods = append(pods, &propPod{
				name:     rev + "-" + strconv.Itoa(i),
				ip:       "10.0." + strconv.Itoa(r+1) + "." + strconv.Itoa(i),
				revision: rev,
			})
		}
	}
	for i := range 2 {
		pods = append(pods, &propPod{
			name: "activator-" + strconv.Itoa(i),
			ip:   "10.9.0." + strconv.Itoa(i),
		})
	}
	return pods
}

func TestColdFlipInvariantProperty(t *testing.T) {
	for seed := range 10 {
		t.Run("seed-"+strconv.Itoa(seed), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewPCG(uint64(seed), 42))
			for range 25 {
				runSequence(t, rng, 40)
			}
		})
	}
}

func runSequence(t *testing.T, rng *rand.Rand, events int) {
	t.Helper()
	ctx := t.Context()
	client := fake.NewClientset(
		revisionService(propRevisions[0], "web"),
		revisionService(propRevisions[1], "web"),
	)
	r := New(client, testNS, testOptions())
	pods := newUniverse()

	for step := range events {
		desc := applyRandomEvent(t, client, rng, pods)
		for _, rev := range propRevisions {
			if err := r.reconcileService(ctx, "dep-"+rev); err != nil {
				t.Fatalf("step %d (%s): reconcile %s: %v", step, desc, rev, err)
			}
			assertInvariant(t, client, pods, rev, step, desc)
		}
	}
}

// applyRandomEvent mutates one random pod: absent pods are created (ready or
// not); present pods flip readiness, start terminating, or get deleted.
func applyRandomEvent(t *testing.T, client *fake.Clientset, rng *rand.Rand, pods []*propPod) string {
	t.Helper()
	ctx := t.Context()
	p := pods[rng.IntN(len(pods))]
	api := client.CoreV1().Pods(testNS)

	if !p.exists {
		p.exists, p.ready, p.terminating = true, rng.IntN(2) == 0, false
		if _, err := api.Create(ctx, p.toK8s(), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", p.name, err)
		}
		return "add " + p.name + " ready=" + strconv.FormatBool(p.ready)
	}

	switch rng.IntN(4) {
	case 0:
		p.exists = false
		if err := api.Delete(ctx, p.name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %s: %v", p.name, err)
		}
		return "delete " + p.name
	case 1:
		p.ready = true
	case 2:
		p.ready = false
	default:
		p.terminating = true
	}
	if _, err := api.Update(ctx, p.toK8s(), metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update %s: %v", p.name, err)
	}
	return "update " + p.name + " ready=" + strconv.FormatBool(p.ready) + " terminating=" + strconv.FormatBool(p.terminating)
}

func assertInvariant(t *testing.T, client *fake.Clientset, pods []*propPod, rev string, step int, desc string) {
	t.Helper()

	var want, readyActivators []string
	for _, p := range pods {
		if !p.memberReady() {
			continue
		}
		switch p.revision {
		case rev:
			want = append(want, p.ip)
		case "":
			readyActivators = append(readyActivators, p.ip)
		}
	}
	wantPort := testProxyPort
	if len(want) == 0 {
		want = readyActivators
		wantPort = testActivatorPort
	}
	slices.Sort(want)

	slice := getSlice(t, client, "dep-"+rev)
	if got := sliceIPs(slice); !slices.Equal(got, want) {
		t.Fatalf("step %d (%s) %s: endpoints = %v, want %v", step, desc, rev, got, want)
	}
	if got := slicePort(t, slice); got != wantPort {
		t.Fatalf("step %d (%s) %s: port = %d, want %d", step, desc, rev, got, wantPort)
	}
	if len(readyActivators) > 0 && len(slice.Endpoints) == 0 {
		t.Fatalf("step %d (%s) %s: slice empty while a ready activator exists", step, desc, rev)
	}
}
