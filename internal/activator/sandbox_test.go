package activator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/proxy"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// sandboxOpts is what a test needs to wire the sandbox proxy: the pod ports its
// fake backend listens on, and how long the broker may hold a request.
type sandboxOpts struct {
	proxyPort int32
	adminPort int32
	hold      time.Duration
}

const (
	testSandboxDomain = "sandboxes.example.test"
	testTokenLabel    = "sandbox.token"
	testToken         = "9f3c1a04b7e28d65f1024c8ba3e7d95f"
)

// newTestSandboxProxy wires the sandbox proxy over the Kubernetes target
// resolver, the way cmd/sandbox-proxy does.
func newTestSandboxProxy(t *testing.T, opts sandboxOpts, objs ...runtime.Object) *SandboxProxy {
	t.Helper()
	targets := NewPodTargets(fake.NewClientset(objs...), PodTargetsConfig{
		Namespace:  testRevisionNamespace,
		ManagedBy:  revisionManagedByValue,
		TokenLabel: testTokenLabel,
		ProxyPort:  opts.proxyPort,
		AdminPort:  opts.adminPort,
	})
	if err := targets.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return NewSandboxProxy(targets, SandboxConfig{Domain: testSandboxDomain, Hold: opts.hold}, nil)
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
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: time.Second},
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
	act := newTestSandboxProxy(t, sandboxOpts{hold: 50 * time.Millisecond})

	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, sandboxRequest(t, "s-deadbeef."+testSandboxDomain))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSandboxEdge_ForeignHost404(t *testing.T) {
	act := newTestSandboxProxy(t, sandboxOpts{hold: time.Second})

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
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: 2 * time.Second},
		sandboxPod(testToken, "sbx-1", ip, false)) // Running, not yet Ready

	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, sandboxRequest(t, "s-"+testToken+"."+testSandboxDomain))

	if rec.Code != http.StatusOK || rec.Body.String() != "served" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

// On the Docker backend both data planes share one listener, so Matches decides
// which of them serves a request — and both domains default to the same value.
// A host that merely shares the sandbox domain has to fall through to the
// deployments plane instead of being read as a capability token.
func TestSandboxEdge_DeclinesANeighbouringHost(t *testing.T) {
	act := newTestSandboxProxy(t, sandboxOpts{hold: time.Second})

	for _, host := range []string{"myapp." + testSandboxDomain, "myapp.other.test"} {
		if act.Matches(host) {
			t.Errorf("%q matched — the deployments plane would never see it", host)
		}
		rec := httptest.NewRecorder()
		act.ServeHTTP(rec, sandboxRequest(t, host))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%q: status = %d, want 404", host, rec.Code)
		}
	}
	if !act.Matches("s-" + testToken + "." + testSandboxDomain) {
		t.Error("a sandbox host must still match")
	}
}

// Host matching is case-insensitive (DNS is), and a port on the Host header
// must not defeat it.
func TestSandboxEdge_HostNormalization(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: time.Second},
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

// Every path belongs to the sandbox. The proxy must not answer any of them
// itself — mounting its own /healthz on the data listener once shadowed the
// contract's /healthz for every sandbox behind it (its own probes live on the
// management listener instead).
func TestSandboxEdge_ClaimsNoPathOfItsOwn(t *testing.T) {
	var seen []string
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		_, _ = io.WriteString(w, "sandbox")
	}))
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: time.Second},
		sandboxPod(testToken, "sbx-1", ip, true))

	for _, path := range []string{"/healthz", "/execute", "/files/main.py", "/stats", "/ready", "/"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"http://s-"+testToken+"."+testSandboxDomain+path, nil)
		req.Host = "s-" + testToken + "." + testSandboxDomain
		rec := httptest.NewRecorder()
		act.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "sandbox" {
			t.Errorf("%s: the proxy answered instead of the sandbox (status %d, body %q)", path, rec.Code, rec.Body.String())
		}
	}
	if len(seen) != 6 {
		t.Errorf("sandbox saw %v", seen)
	}
}

// A sandbox's extra ports are addressed by hostname, and the port rides in the
// same label as the token: one wildcard cert covers every port of every
// sandbox.
func TestSandboxEdge_PortLabelBecomesThePortHint(t *testing.T) {
	var hints []string
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hints = append(hints, r.Header.Get(proxy.HeaderPort))
		w.WriteHeader(http.StatusOK)
	}))
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: time.Second},
		sandboxPod(testToken, "sbx-1", ip, true))

	for _, tc := range []struct{ host, want string }{
		{"s-" + testToken + "." + testSandboxDomain, ""},          // the pool's own port
		{"s-" + testToken + "-5173." + testSandboxDomain, "5173"}, // a declared extra
	} {
		rec := httptest.NewRecorder()
		act.ServeHTTP(rec, sandboxRequest(t, tc.host))
		if rec.Code != http.StatusOK {
			t.Fatalf("host %q: status = %d", tc.host, rec.Code)
		}
	}
	if len(hints) != 2 || hints[0] != "" || hints[1] != "5173" {
		t.Errorf("port hints reaching the sandbox: got %q", hints)
	}
}

// The hint is the hostname's to give. A client that sets the header itself must
// not reach a port its URL did not name.
func TestSandboxEdge_StripsClientSuppliedPortHint(t *testing.T) {
	var hints []string
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hints = append(hints, r.Header.Get(proxy.HeaderPort))
		w.WriteHeader(http.StatusOK)
	}))
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: time.Second},
		sandboxPod(testToken, "sbx-1", ip, true))

	req := sandboxRequest(t, "s-"+testToken+"."+testSandboxDomain)
	req.Header.Set(proxy.HeaderPort, "8001") // the sidecar's admin port
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if len(hints) != 1 || hints[0] != "" {
		t.Errorf("client-supplied hint survived: got %q", hints)
	}
}

// A token containing a hyphenated tail that is not a port must not be mistaken
// for one — the sandbox would be unreachable.
func TestSandboxEdge_NonNumericSuffixIsPartOfTheToken(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	act := newTestSandboxProxy(t, sandboxOpts{proxyPort: port, adminPort: port, hold: time.Second},
		sandboxPod("abc-def", "sbx-1", ip, true))

	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, sandboxRequest(t, "s-abc-def."+testSandboxDomain))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
