package activator

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"orchestrator/internal/proxy"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// The sandbox edge is the third edge over the same broker, and the cheapest:
// one wildcard HTTPRoute for *.{domain} sends every sandbox here, and the
// leading DNS label of the request's Host carries the sandbox's capability
// token (and, optionally, which of its ports) — no resolve, no id→token
// indirection, no cache. Per-sandbox routes were the alternative and do not
// survive the churn: a new HTTPRoute is not live until the gateway programs it,
// often seconds, so a create would hand back a URL that 503s for longer than
// the sub-second claim it was built to avoid.
const (
	// sandboxHostPrefix leads every sandbox hostname (internal/sandbox).
	sandboxHostPrefix = "s-"

	// defaultSandboxHold bounds the wait for a claimed sandbox's pod to answer.
	// Seconds, not the deployments StartTimeout: a sandbox has no
	// scale-from-zero, so the only legitimate wait is the tail of its own
	// creation (the pod exists, artifacts are materializing). If the pod is
	// gone, the sandbox is gone, and waiting cannot fix it.
	defaultSandboxHold = 5 * time.Second

	sandboxInformerResync = 30 * time.Second
)

// SandboxConfig configures the SandboxActivator.
type SandboxConfig struct {
	Namespace string
	// Domain is the wildcard sandbox domain; hosts are s-{token}.{Domain}, or
	// s-{token}-{port}.{Domain} for a sandbox's extra ports.
	Domain string
	// ManagedBy and TokenLabel are the sandbox backend's label contract
	// (internal/sandbox/kubernetes).
	ManagedBy  string
	TokenLabel string
	ProxyPort  int32 // sandbox pod data port (proxy.DefaultProxyPort)
	AdminPort  int32 // sandbox pod admin port for direct probing (proxy.DefaultAdminPort)
	Hold       time.Duration
}

// SandboxActivator is the sandbox data-plane edge: it resolves the capability
// token from the request's Host and proxies to the pod carrying that token.
//
// Sync only. Async delivery is deployment-typed (its callback and event triple
// come off a deployment spec), and a sandbox has no need for it: async
// execution belongs to the image's own /execute contract, not to us. Staying
// sync also keeps file transfers streaming rather than buffered.
type SandboxActivator struct {
	client kubernetes.Interface
	broker *broker
	cfg    SandboxConfig

	pods corelisters.PodLister
}

// NewSandboxActivator creates a SandboxActivator. rec (nilable) receives the
// hold metrics. Call Start before serving.
func NewSandboxActivator(client kubernetes.Interface, cfg SandboxConfig, rec Recorder) *SandboxActivator {
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = proxy.DefaultProxyPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = proxy.DefaultAdminPort
	}
	if cfg.Hold <= 0 {
		cfg.Hold = defaultSandboxHold
	}
	return &SandboxActivator{
		client: client,
		broker: newBroker(nil, rec, edgeSandbox),
		cfg:    cfg,
	}
}

// Start runs the sandbox pod informer on ctx and blocks until its cache syncs;
// the informer keeps running until ctx cancels.
func (a *SandboxActivator) Start(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(a.client, sandboxInformerResync,
		informers.WithNamespace(a.cfg.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = revisionLabelManagedBy + "=" + a.cfg.ManagedBy + "," + a.cfg.TokenLabel
		}),
	)
	pods := factory.Core().V1().Pods()
	a.pods = pods.Lister()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pods.Informer().HasSynced) {
		return errors.New("informer caches failed to sync")
	}
	return nil
}

// ServeHTTP implements the sandbox data plane. The token is read from the Host
// and never logged or echoed: it is the credential, so an error body says only
// that nothing answers there.
func (a *SandboxActivator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, port := a.resolve(hostOnly(r.Host))
	if token == "" {
		http.Error(w, "not a sandbox host", http.StatusNotFound)
		return
	}

	// The port hint is derived from the hostname, never accepted from the
	// client: an inbound copy is dropped, so a caller cannot reach a port the
	// hostname did not name (and the sidecar refuses any the claim did not
	// declare either).
	r.Header.Del(proxy.HeaderPort)
	if port != "" {
		r.Header.Set(proxy.HeaderPort, port)
	}
	a.broker.sync(w, r, token, r.Host, a.cfg.Hold, sandboxCapacity{a: a, token: token})
}

// resolve splits a sandbox host into its capability token and (optionally) the
// port it addresses: s-{token}.{domain} is the pool's own port, and
// s-{token}-{port}.{domain} is one of the sandbox's declared extras. Both live
// in ONE DNS label because a wildcard certificate covers exactly one (RFC
// 6125) — nesting the port as its own label would need a cert per sandbox.
// Returns ("", "") when the host is not one of ours.
func (a *SandboxActivator) resolve(host string) (token, port string) {
	label, domain, ok := strings.Cut(host, ".")
	if !ok || !strings.EqualFold(domain, a.cfg.Domain) {
		return "", ""
	}
	label = strings.TrimPrefix(label, sandboxHostPrefix)
	if base, suffix, ok := strings.Cut(label, "-"); ok && isPort(suffix) {
		return base, suffix
	}
	return label, ""
}

// isPort reports whether s is a plausible port number, so a hyphen inside a
// token (or a trailing word) is not mistaken for one.
func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535
}

// sandboxCapacity adapts one sandbox to the broker's seam: the target is the
// pod labelled with this token, and there is nothing to raise — a sandbox is a
// claimed pod, so if the pod is gone the sandbox is gone.
type sandboxCapacity struct {
	a     *SandboxActivator
	token string
}

func (c sandboxCapacity) Target(ctx context.Context) (*url.URL, error) {
	selector := labels.SelectorFromSet(labels.Set{c.a.cfg.TokenLabel: c.token})
	pods, err := c.a.pods.Pods(c.a.cfg.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	if target := readyPodTarget(pods, c.a.cfg.ProxyPort); target != nil {
		return target, nil
	}
	// Still creating: the pod exists and its sidecar may already be serving
	// ahead of kubelet readiness propagation.
	return probeCandidates(ctx, pods, c.a.cfg.ProxyPort, c.a.cfg.AdminPort), nil
}

func (c sandboxCapacity) Raise(context.Context) error { return nil }
