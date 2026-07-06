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
	"orchestrator/pkg/deployment"
	"strconv"
	"strings"
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
	broker *broker
	cfg    RevisionConfig

	pods        corelisters.PodLister
	deployments appslisters.DeploymentLister
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
		client: client,
		broker: newBroker(queue),
		cfg:    cfg,
	}
}

// QueuedByRevision snapshots how many requests are waiting for each
// revision's first pod — the autoscaler's hold-up signal while a cold start
// is in flight (scraped via GET /stats on the data listener).
func (a *RevisionActivator) QueuedByRevision() map[string]int {
	return a.broker.depths()
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
// the target revision, then the request is handed to the broker with that
// revision's capacity bound in. Sync requests never read the spec — only
// async needs the callback config from the Spec Secret.
func (a *RevisionActivator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rev := r.Header.Get(headerRevision)
	if rev == "" {
		http.Error(w, "missing "+headerRevision+" header", http.StatusBadRequest)
		return
	}
	r.Header.Del(headerRevision)

	c := revisionCapacity{a: a, rev: rev}

	if proxy.PreferAsync(r) {
		spec, err := a.specFor(r.Context(), rev)
		if err != nil {
			http.Error(w, "no deployment for revision "+rev, http.StatusNotFound)
			return
		}
		a.broker.async(w, r, rev, r.Host, spec, a.cfg.ResponseStartTimeout, c)
		return
	}
	a.broker.sync(w, r, rev, r.Host, a.cfg.ResponseStartTimeout, c)
}

// revisionCapacity adapts one revision to the broker's seam: targets are the
// informer's ready pods — or, during a cold start, the first Running pod
// whose sidecar answers a direct /ready probe (the Knative activator move,
// skipping kubelet readiness propagation) — and a raise patches the revision
// Deployment's scale subresource 0→1.
type revisionCapacity struct {
	a   *RevisionActivator
	rev string
}

func (c revisionCapacity) Target(ctx context.Context) (*url.URL, error) {
	selector := labels.SelectorFromSet(labels.Set{revisionLabel: c.rev})
	pods, err := c.a.pods.Pods(c.a.cfg.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	if target := readyPodTarget(pods, c.a.cfg.ProxyPort); target != nil {
		return target, nil
	}
	return c.a.probeCandidates(ctx, pods), nil
}

func (c revisionCapacity) Raise(ctx context.Context) error {
	name := revisionDeploymentName(c.rev)
	dep, err := c.a.deployments.Deployments(c.a.cfg.Namespace).Get(name)
	if err != nil {
		return fmt.Errorf("raise skipped: %w", err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		return nil // already raised; pods are on their way
	}

	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.a.cfg.Namespace},
		Spec:       autoscalingv1.ScaleSpec{Replicas: 1},
	}
	if _, err := c.a.client.AppsV1().Deployments(c.a.cfg.Namespace).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		return err
	}
	slog.Info("Cold-start scale-up requested", "revision", c.rev)
	return nil
}

// probeCandidates direct-probes the sidecar /ready of the revision's Running
// (ready or not) pods, releasing the request to the first responder.
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
