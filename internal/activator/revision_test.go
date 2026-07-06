package activator

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"orchestrator/pkg/deployment"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testRevisionNamespace = "orchestrator"

// registerScaleSubresource teaches the fake clientset the deployments/scale
// subresource, which it does not implement natively (GetScale/UpdateScale
// panic casting the tracked *apps/v1.Deployment to *autoscaling/v1.Scale).
// Copied from internal/deployment/kubernetes's tests.
func registerScaleSubresource(cs *fake.Clientset) {
	gvr := appsv1.SchemeGroupVersion.WithResource("deployments")
	cs.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(gvr, get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		dep := obj.(*appsv1.Deployment)
		replicas := int32(1)
		if dep.Spec.Replicas != nil {
			replicas = *dep.Spec.Replicas
		}
		return true, &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: dep.Namespace},
			Spec:       autoscalingv1.ScaleSpec{Replicas: replicas},
		}, nil
	})
	cs.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(k8stesting.UpdateAction)
		if !ok || action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		scale := update.GetObject().(*autoscalingv1.Scale)
		obj, err := cs.Tracker().Get(gvr, update.GetNamespace(), scale.Name)
		if err != nil {
			return true, nil, err
		}
		dep := obj.(*appsv1.Deployment)
		dep.Spec.Replicas = &scale.Spec.Replicas
		if err := cs.Tracker().Update(gvr, dep, update.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, scale, nil
	})
}

// newTestRevisionActivator builds a RevisionActivator over a fake clientset
// seeded with objs and blocks until its informers have synced.
func newTestRevisionActivator(t *testing.T, cfg RevisionConfig, objs ...runtime.Object) (*RevisionActivator, *fake.Clientset, *captureQueue) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	registerScaleSubresource(cs)
	queue := newCaptureQueue()
	cfg.Namespace = testRevisionNamespace
	act := NewRevisionActivator(cs, queue, cfg, nil)
	if err := act.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return act, cs, queue
}

// revisionBackend runs an httptest server standing in for a workload pod and
// returns its IP and port (used as both data and admin port in tests).
func revisionBackend(t *testing.T, handler http.Handler) (ip string, port int32) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split backend host: %v", err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}
	return host, int32(p)
}

func revisionPod(rev, name, ip string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testRevisionNamespace,
			Labels: map[string]string{
				revisionLabelManagedBy: revisionManagedByValue,
				revisionLabel:          rev,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

func revisionDeployment(t *testing.T, rev string, replicas int32, spec *deployment.Request) *appsv1.Deployment {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionDeploymentName(rev),
			Namespace: testRevisionNamespace,
			Labels:    map[string]string{revisionLabelManagedBy: revisionManagedByValue},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
	}
	if spec != nil {
		t.Fatal("seed the spec via revisionSpecSecret, not the Deployment (the spec lives on the dep-{id} Secret)")
	}
	return dep
}

// revisionSpecSecret builds the per-deployment dep-{id} Secret carrying the
// spec — where the revision activator reads callback/timeout config from.
func revisionSpecSecret(t *testing.T, rev string, spec *deployment.Request) *corev1.Secret {
	t.Helper()
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionDeploymentName(deploymentIDOf(rev)),
			Namespace: testRevisionNamespace,
			Labels:    map[string]string{revisionLabelManagedBy: revisionManagedByValue},
		},
		Data: map[string][]byte{specSecretKey: specJSON},
	}
}

func TestRevision_MissingHeader400(t *testing.T) {
	act, _, _ := newTestRevisionActivator(t, RevisionConfig{StartTimeout: time.Second})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test/", nil)
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRevision_WarmForwardsToReadyPod(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "app.example.test" {
			t.Errorf("backend saw Host %q, want app.example.test", r.Host)
		}
		if r.Header.Get(headerRevision) != "" {
			t.Error("X-Revision leaked to the workload")
		}
		_, _ = io.WriteString(w, "hello")
	}))
	act, _, _ := newTestRevisionActivator(t,
		RevisionConfig{ProxyPort: port, AdminPort: port, StartTimeout: time.Second},
		revisionPod("rev1", "pod-1", ip, true))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test/x", nil)
	req.Header.Set(headerRevision, "rev1")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", rec.Body.String())
	}
}

func TestRevision_ColdRaisesScaleAndProbes(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, "warmed")
	}))
	act, cs, _ := newTestRevisionActivator(t,
		RevisionConfig{ProxyPort: port, AdminPort: port, StartTimeout: 5 * time.Second},
		revisionDeployment(t, "rev1", 0, nil))

	// When the scale patch lands, a Running-but-NOT-ready pod appears: the
	// direct sidecar probe, not kubelet readiness, must release the request.
	cs.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "scale" {
			_ = cs.Tracker().Add(revisionPod("rev1", "pod-cold", ip, false))
		}
		return false, nil, nil // fall through to the scale reactor
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test/", nil)
	req.Header.Set(headerRevision, "rev1")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "warmed" {
		t.Fatalf("cold start: status=%d body=%q", rec.Code, rec.Body.String())
	}

	dep, err := cs.AppsV1().Deployments(testRevisionNamespace).Get(t.Context(), "dep-rev1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1 (scale subresource patched)", dep.Spec.Replicas)
	}
}

func TestRevision_ColdTimeout503(t *testing.T) {
	act, _, _ := newTestRevisionActivator(t,
		RevisionConfig{StartTimeout: 100 * time.Millisecond},
		revisionDeployment(t, "rev1", 0, nil))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://app.example.test/", nil)
	req.Header.Set(headerRevision, "rev1")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRevisionAsync_AcceptsAndDeliversCallback(t *testing.T) {
	ip, port := revisionBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "payload" {
			t.Errorf("backend body = %q, want payload", body)
		}
		if r.Header.Get("Prefer") != "" {
			t.Error("Prefer header leaked to the workload")
		}
		if r.Header.Get(headerRevision) != "" {
			t.Error("X-Revision leaked to the workload")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "done")
	}))
	spec := &deployment.Request{
		ID:             "app",
		Host:           "app.example.test",
		Port:           8080,
		TimeoutSeconds: 5,
		Callback:       &deployment.Callback{URL: "http://callbacks.test/hook", Key: "k"},
	}
	act, _, queue := newTestRevisionActivator(t,
		RevisionConfig{ProxyPort: port, AdminPort: port, StartTimeout: time.Second},
		revisionPod("rev1", "pod-1", ip, true),
		revisionDeployment(t, "rev1", 1, nil), revisionSpecSecret(t, "rev1", spec))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.test/", strings.NewReader("payload"))
	req.Header.Set("Prefer", "respond-async")
	req.Header.Set(headerRevision, "rev1")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	invocationID := rec.Header().Get("X-Invocation-Id")
	if invocationID == "" {
		t.Fatal("missing X-Invocation-Id")
	}

	select {
	case <-queue.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no callback dispatched")
	}

	event := queue.last()
	if event.Destination != "http://callbacks.test/hook" {
		t.Fatalf("callback destination = %q", event.Destination)
	}
	if event.Payload.Type != "orchestrator.deployment.response" {
		t.Fatalf("event type = %q", event.Payload.Type)
	}
	if event.Payload.Data["deploymentId"] != "app" {
		t.Fatalf("deploymentId = %v, want app", event.Payload.Data["deploymentId"])
	}
	if event.Payload.Data["invocationId"] != invocationID {
		t.Fatalf("invocationId mismatch: %v vs %s", event.Payload.Data["invocationId"], invocationID)
	}
	if event.Payload.Data["statusCode"] != http.StatusCreated {
		t.Fatalf("statusCode = %v, want 201", event.Payload.Data["statusCode"])
	}
	if event.Payload.Data["body"] != "done" {
		t.Fatalf("body = %v, want done", event.Payload.Data["body"])
	}
}

func TestRevisionAsync_RequiresCallback(t *testing.T) {
	// A forged async request to a revision whose deployment has no callback
	// must be rejected, not silently dropped.
	spec := &deployment.Request{ID: "app", Host: "app.example.test", Port: 8080}
	act, _, _ := newTestRevisionActivator(t,
		RevisionConfig{StartTimeout: time.Second},
		revisionDeployment(t, "rev1", 1, nil), revisionSpecSecret(t, "rev1", spec))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://app.example.test/", strings.NewReader("{}"))
	req.Header.Set("Prefer", "respond-async")
	req.Header.Set(headerRevision, "rev1")
	rec := httptest.NewRecorder()
	act.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
