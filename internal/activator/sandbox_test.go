package activator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testSandboxDomain = "sandboxes.example.test"
	testTokenLabel    = "sandbox.token"
	testToken         = "9f3c1a04b7e28d65f1024c8ba3e7d95f"
)

func newTestSandboxActivator(t *testing.T, cfg SandboxConfig, objs ...runtime.Object) *SandboxActivator {
	t.Helper()
	cfg.Namespace = testRevisionNamespace
	cfg.Domain = testSandboxDomain
	cfg.ManagedBy = revisionManagedByValue
	cfg.TokenLabel = testTokenLabel
	act := NewSandboxActivator(fake.NewClientset(objs...), cfg, nil)
	if err := act.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return act
}

func sandboxPod(token, name, ip string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testRevisionNamespace,
			Labels: map[string]string{
				revisionLabelManagedBy: revisionManagedByValue,
				testTokenLabel:         token,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func sandboxRequest(t *testing.T, host string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+"/execute", nil)
	req.Host = host
	return req
}

// The Host's leading label IS the token — no lookup, no id→token indirection.
func TestSandboxEdge_ForwardsByHostToken(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/execute" {
			t.Errorf("backend saw path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"exitCode":0}`)
	}))
	act := newTestSandboxActivator(t, SandboxConfig{ProxyPort: port, AdminPort: port, Hold: time.Second},
		sandboxPod(testToken, "sbx-1", ip, true))

	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, sandboxRequest(t, "s-"+testToken+"."+testSandboxDomain))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != `{"exitCode":0}` {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// A guessed or stale token reaches nothing, and the wait is bounded by Hold —
// there is no scale-from-zero to wait for.
func TestSandboxEdge_UnknownToken503(t *testing.T) {
	act := newTestSandboxActivator(t, SandboxConfig{Hold: 50 * time.Millisecond})

	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, sandboxRequest(t, "s-deadbeef."+testSandboxDomain))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSandboxEdge_ForeignHost404(t *testing.T) {
	act := newTestSandboxActivator(t, SandboxConfig{Hold: time.Second})

	for _, host := range []string{"app.example.com", "sandboxes.example.test", "s-abc.other.example"} {
		rec := httptest.NewRecorder()
		act.ServeHTTP(rec, sandboxRequest(t, host))
		if rec.Code != http.StatusNotFound {
			t.Errorf("host %q: status = %d, want 404", host, rec.Code)
		}
	}
}

// A sandbox still creating is reachable as soon as its sidecar answers, ahead
// of kubelet readiness propagation — that wait is most of a sub-second claim.
func TestSandboxEdge_CreatingPodReachedByDirectProbe(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "served")
	}))
	act := newTestSandboxActivator(t, SandboxConfig{ProxyPort: port, AdminPort: port, Hold: 2 * time.Second},
		sandboxPod(testToken, "sbx-1", ip, false)) // Running, not yet Ready

	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, sandboxRequest(t, "s-"+testToken+"."+testSandboxDomain))

	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// Host matching is case-insensitive (DNS is), and a port on the Host header
// must not defeat it.
func TestSandboxEdge_HostNormalization(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	act := newTestSandboxActivator(t, SandboxConfig{ProxyPort: port, AdminPort: port, Hold: time.Second},
		sandboxPod(testToken, "sbx-1", ip, true))

	for _, host := range []string{
		"s-" + testToken + "." + testSandboxDomain + ":8081",
		"s-" + testToken + ".SANDBOXES.EXAMPLE.TEST",
	} {
		rec := httptest.NewRecorder()
		act.ServeHTTP(rec, sandboxRequest(t, host))
		if rec.Code != http.StatusOK {
			t.Errorf("host %q: status = %d, want 200", host, rec.Code)
		}
	}
}
