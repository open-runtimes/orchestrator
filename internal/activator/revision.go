package activator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/cloudevent"
	"orchestrator/pkg/deployment"
	"strconv"
	"strings"
	"sync"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// Label/annotation contract stamped by the deployments-service Kubernetes
// backend onto revision Deployments and their pods. Kept as local literals so
// the data-plane edge does not import the control-plane backend package.
const (
	revisionLabelManagedBy = "managed-by"
	revisionManagedByValue = "deployments-service"
	revisionLabel          = "deployment.revision"
	// specSecretKey is the data key on the per-deployment Secret (named
	// dep-{deploymentID}) holding the head revision's spec JSON — where the
	// callback/timeout config for async requests lives. A Secret, not the
	// marker ConfigMap: the spec carries the callback signing key.
	specSecretKey = "spec"

	// headerRevision is set by the gateway per weighted backendRef — the
	// activator never re-derives the traffic split. Trusted, so ingress is
	// gateway-only (network policy); stripped before forwarding.
	headerRevision = "X-Revision"

	// probeTimeout bounds each direct sidecar /ready probe.
	probeTimeout = 500 * time.Millisecond

	defaultResponseStartTimeout = 300 * time.Second

	revisionInformerResync = 30 * time.Second
)

// RevisionConfig configures the RevisionActivator.
type RevisionConfig struct {
	Namespace            string
	ProxyPort            int32         // workload pod data port (proxy.DefaultProxyPort)
	AdminPort            int32         // workload pod admin port for direct probing (proxy.DefaultAdminPort)
	ResponseStartTimeout time.Duration // wait for the first reachable pod → 503; default 300s
}

// RevisionActivator is the K8s data-plane edge: the gateway routes cold/async
// traffic here with X-Revision set per backendRef; it forwards to that
// revision's ready workload pods directly — never via the routable Service,
// whose endpoints are this activator during the cold window (loop).
type RevisionActivator struct {
	client kubernetes.Interface
	queue  dispatcher.Queue
	cfg    RevisionConfig
	source string // CloudEvents source

	pods        corelisters.PodLister
	deployments appslisters.DeploymentLister

	mu        sync.Mutex
	lastRaise map[string]time.Time // revision → last cold scale-up
	queued    map[string]int       // revision → requests waiting for a pod (the scale-from-zero hold-up signal)
}

// NewRevisionActivator creates a RevisionActivator. queue delivers async
// response callbacks. Call Start before serving.
func NewRevisionActivator(client kubernetes.Interface, queue dispatcher.Queue, cfg RevisionConfig) *RevisionActivator {
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = proxy.DefaultProxyPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = proxy.DefaultAdminPort
	}
	if cfg.ResponseStartTimeout <= 0 {
		cfg.ResponseStartTimeout = defaultResponseStartTimeout
	}
	return &RevisionActivator{
		client:    client,
		queue:     queue,
		cfg:       cfg,
		source:    "orchestrator/deployments",
		lastRaise: make(map[string]time.Time),
		queued:    make(map[string]int),
	}
}

// QueuedByRevision snapshots how many requests are waiting for each
// revision's first pod — the autoscaler's hold-up signal while a cold start
// is in flight (scraped via GET /stats on the data listener).
func (a *RevisionActivator) QueuedByRevision() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.queued))
	for rev, n := range a.queued {
		if n > 0 {
			out[rev] = n
		}
	}
	return out
}

func (a *RevisionActivator) trackQueued(rev string, delta int) {
	a.mu.Lock()
	a.queued[rev] += delta
	if a.queued[rev] <= 0 {
		delete(a.queued, rev)
	}
	a.mu.Unlock()
}

// Start runs the managed pod + Deployment informers on ctx and blocks until
// their caches sync; the informers keep running until ctx cancels.
func (a *RevisionActivator) Start(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(a.client, revisionInformerResync,
		informers.WithNamespace(a.cfg.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = revisionLabelManagedBy + "=" + revisionManagedByValue
		}),
	)
	pods := factory.Core().V1().Pods()
	deps := factory.Apps().V1().Deployments()
	a.pods = pods.Lister()
	a.deployments = deps.Lister()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pods.Informer().HasSynced, deps.Informer().HasSynced) {
		return errors.New("informer caches failed to sync")
	}
	return nil
}

// ServeHTTP implements the data plane: the gateway's X-Revision header names
// the target revision, then the request is proxied synchronously or accepted
// for async delivery.
func (a *RevisionActivator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rev := r.Header.Get(headerRevision)
	if rev == "" {
		http.Error(w, "missing "+headerRevision+" header", http.StatusBadRequest)
		return
	}
	r.Header.Del(headerRevision)

	// Exact-literal match by design; combined RFC 7240 forms are not recognized.
	if r.Header.Get("Prefer") == "respond-async" {
		a.serveAsync(w, r, rev)
		return
	}
	a.serveSync(w, r, rev)
}

// serveSync waits for a reachable pod of the revision (bounded by
// ResponseStartTimeout) and proxies the request to it, preserving the inbound
// Host — the workload's virtual host. The per-request 504 timeout is enforced
// by the deployments-sidecar, not here.
func (a *RevisionActivator) serveSync(w http.ResponseWriter, r *http.Request, rev string) {
	target, err := a.waitForPod(r.Context(), rev)
	if err != nil {
		http.Error(w, "no serving capacity became ready", http.StatusServiceUnavailable)
		return
	}
	proxyTo(target, r.Host).ServeHTTP(w, r)
}

// serveAsync buffers the request, responds 202 immediately, and delivers the
// eventual response to the deployment's callback as a CloudEvent. Delivery is
// at-most-once: nothing is stored, X-Invocation-Id is a correlation id only.
func (a *RevisionActivator) serveAsync(w http.ResponseWriter, r *http.Request, rev string) {
	spec, err := a.specFor(r.Context(), rev)
	if err != nil {
		http.Error(w, "no deployment for revision "+rev, http.StatusNotFound)
		return
	}
	if spec.Callback == nil || spec.Callback.URL == "" {
		http.Error(w, "async requires a callback on the deployment", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAsyncRequestBody+1))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxAsyncRequestBody {
		http.Error(w, "async request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	invocationID := newInvocationID()
	req := cloneForForward(r, r.Host, body)

	w.Header().Set("X-Invocation-Id", invocationID)
	w.WriteHeader(http.StatusAccepted)

	go a.forwardAsync(req, rev, spec, invocationID)
}

// forwardAsync executes the buffered request against the revision and
// dispatches the orchestrator.deployment.response CloudEvent.
func (a *RevisionActivator) forwardAsync(r *http.Request, rev string, spec *deployment.Request, invocationID string) {
	ctx, cancel := context.WithTimeout(context.Background(),
		a.cfg.ResponseStartTimeout+time.Duration(spec.TimeoutSeconds)*time.Second)
	defer cancel()

	status, body, truncated, errMsg := a.forward(ctx, r, rev)
	if errMsg != "" {
		slog.Warn("Async forward failed", "revision", rev, "invocationId", invocationID, "error", errMsg)
	}

	data := map[string]any{
		"deploymentId": spec.ID,
		"invocationId": invocationID,
	}
	if status > 0 {
		data["statusCode"] = status
	}
	if body != nil {
		data["body"] = string(body)
		data["bodyTruncated"] = truncated
	}
	if errMsg != "" {
		data["error"] = errMsg
	}

	event := cloudevent.New("orchestrator.deployment.response", a.source, spec.ID, invocationID, data)
	if err := a.queue.Dispatch(&dispatcher.Event{
		Payload:     event,
		Destination: spec.Callback.URL,
		SigningKey:  spec.Callback.Key,
	}); err != nil {
		slog.Warn("Failed to dispatch async response", "revision", rev, "invocationId", invocationID, "error", err)
	}
}

// forward sends the buffered request to a reachable pod of the revision and
// reads the response, capped at maxCallbackResponseBody (larger bodies are
// truncated and flagged).
func (a *RevisionActivator) forward(ctx context.Context, r *http.Request, rev string) (status int, body []byte, truncated bool, errMsg string) {
	target, err := a.waitForPod(ctx, rev)
	if err != nil {
		return 0, nil, false, "no serving capacity became ready"
	}

	fwd := r.Clone(ctx)
	fwd.URL.Scheme = target.Scheme
	fwd.URL.Host = target.Host
	fwd.RequestURI = ""

	resp, err := http.DefaultClient.Do(fwd)
	if err != nil {
		return 0, nil, false, "forward failed: " + err.Error()
	}
	defer resp.Body.Close()

	body, err = io.ReadAll(io.LimitReader(resp.Body, maxCallbackResponseBody+1))
	if err != nil {
		return resp.StatusCode, nil, false, "failed to read response: " + err.Error()
	}
	if len(body) > maxCallbackResponseBody {
		return resp.StatusCode, body[:maxCallbackResponseBody], true, ""
	}
	return resp.StatusCode, body, false, ""
}

// waitForPod resolves a forward target for the revision, bounded by
// ResponseStartTimeout: a ready pod from the informer wins immediately; a cold
// revision is raised 0→1 and its Running pods are direct-probed so the first
// reachable sidecar releases the request without waiting on kubelet readiness
// propagation.
func (a *RevisionActivator) waitForPod(ctx context.Context, rev string) (*url.URL, error) {
	waitCtx, cancel := context.WithTimeout(ctx, a.cfg.ResponseStartTimeout)
	defer cancel()

	a.trackQueued(rev, 1)
	defer a.trackQueued(rev, -1)

	selector := labels.SelectorFromSet(labels.Set{revisionLabel: rev})
	for {
		pods, err := a.pods.Pods(a.cfg.Namespace).List(selector)
		if err == nil {
			if target := readyPodTarget(pods, a.cfg.ProxyPort); target != nil {
				return target, nil
			}
			a.raise(waitCtx, rev)
			if target := a.probeCandidates(waitCtx, pods); target != nil {
				return target, nil
			}
		}
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-time.After(endpointPollInterval):
		}
	}
}

// raise patches the revision Deployment's scale subresource 0→1 — the cold
// start the activator owns, never waiting on an autoscaler tick. Debounced per
// revision so concurrent cold hits (and the poll loop) issue one write;
// failures are logged, not returned — the wait carries on and the request
// fails with 503 only if nothing becomes reachable in time.
func (a *RevisionActivator) raise(ctx context.Context, rev string) {
	a.mu.Lock()
	if time.Since(a.lastRaise[rev]) < raiseDebounce {
		a.mu.Unlock()
		return
	}
	a.lastRaise[rev] = time.Now()
	pruneStale(a.lastRaise, raiseDebounce)
	a.mu.Unlock()

	name := revisionDeploymentName(rev)
	dep, err := a.deployments.Deployments(a.cfg.Namespace).Get(name)
	if err != nil {
		slog.Warn("Cold-start raise skipped", "revision", rev, "error", err)
		return
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		return // already raised; pods are on their way
	}

	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.cfg.Namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: 1},
	}
	if _, err := a.client.AppsV1().Deployments(a.cfg.Namespace).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		slog.Warn("Cold-start scale-up failed", "revision", rev, "error", err)
		return
	}
	slog.Info("Cold-start scale-up requested", "revision", rev)
}

// probeCandidates direct-probes the sidecar /ready of the revision's Running
// (ready or not) pods, releasing the request to the first responder — the
// Knative activator move.
func (a *RevisionActivator) probeCandidates(ctx context.Context, pods []*corev1.Pod) *url.URL {
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		if a.probeReady(ctx, pod.Status.PodIP) {
			return podDataTarget(pod, a.cfg.ProxyPort)
		}
	}
	return nil
}

// probeReady checks the sidecar admin /ready endpoint on the pod directly.
func (a *RevisionActivator) probeReady(ctx context.Context, podIP string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	probeURL := "http://" + net.JoinHostPort(podIP, strconv.Itoa(int(a.cfg.AdminPort))) + "/ready"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// specFor reconstructs the deployment spec for a revision from its dep-{id}
// Secret (data key "spec"). Async is the cold path, so a direct GET (no
// informer) is fine.
func (a *RevisionActivator) specFor(ctx context.Context, rev string) (*deployment.Request, error) {
	deploymentID := deploymentIDOf(rev)
	secret, err := a.client.CoreV1().Secrets(a.cfg.Namespace).Get(ctx, revisionDeploymentName(deploymentID), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	raw := secret.Data[specSecretKey]
	if len(raw) == 0 {
		return nil, fmt.Errorf("secret %s missing %s data", secret.Name, specSecretKey)
	}
	var spec deployment.Request
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("secret %s has invalid %s data: %w", secret.Name, specSecretKey, err)
	}
	return &spec, nil
}

// deploymentIDOf strips the -NNNNN revision counter off a revision name.
func deploymentIDOf(rev string) string {
	if i := strings.LastIndex(rev, "-"); i > 0 {
		return rev[:i]
	}
	return rev
}

// readyPodTarget returns the data-port URL of the first ready pod.
func readyPodTarget(pods []*corev1.Pod, port int32) *url.URL {
	for _, pod := range pods {
		if revisionPodReady(pod) && pod.Status.PodIP != "" {
			return podDataTarget(pod, port)
		}
	}
	return nil
}

func podDataTarget(pod *corev1.Pod, port int32) *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))),
	}
}

func revisionPodReady(pod *corev1.Pod) bool {
	// A terminating pod can still report Ready — draining traffic belongs on
	// the surviving pods (same rule as the endpoint-flip reconciler).
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func revisionDeploymentName(rev string) string {
	return "dep-" + rev
}
