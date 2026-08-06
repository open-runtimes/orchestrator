package activator

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/sandbox"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// The sandbox proxy is the third consumer of the same broker, and the cheapest:
// one wildcard HTTPRoute for *.{domain} sends every sandbox here, and the leading
// DNS label of the request's Host carries the sandbox's capability token (and,
// optionally, which of its ports) — no resolve, no id→token indirection, no
// cache. Per-sandbox routes were the alternative and do not survive the churn: a
// new HTTPRoute is not live until the gateway programs it, often seconds, so a
// create would hand back a URL that 503s for longer than the sub-second claim it
// was built to avoid.
//
// It is a PROXY and not an activator: an activator buffers a request and raises
// a workload from zero, and a sandbox has no zero to rise from. What it borrows
// from the activator is the broker's hold-and-forward loop, which covers the one
// legitimate wait — the tail of the sandbox's own creation.
const (
	// defaultSandboxHold bounds the wait for a claimed sandbox's pod to answer.
	// Seconds, not the deployments StartTimeout: with no scale-from-zero, the
	// only legitimate wait is the tail of the sandbox's own creation (the pod
	// exists, artifacts are materializing). If the pod is gone, the sandbox is
	// gone, and waiting cannot fix it.
	defaultSandboxHold = 5 * time.Second

	sandboxInformerResync = 30 * time.Second
)

// SandboxConfig configures the SandboxProxy.
type SandboxConfig struct {
	// Domain is the wildcard sandbox domain; hosts are s-{token}.{Domain}, or
	// s-{token}-{port}.{Domain} for a sandbox's extra ports.
	Domain string
	Hold   time.Duration
}

// SandboxTargets resolves a capability token to the address serving that
// sandbox, or nil when nothing serves it (yet, or ever). The seam is what makes
// this activator backend-neutral: Kubernetes answers from a pod informer, Docker
// from the daemon.
type SandboxTargets interface {
	Target(ctx context.Context, token string) (*url.URL, error)
}

// PodTargetsConfig configures the Kubernetes SandboxTargets.
type PodTargetsConfig struct {
	Namespace string
	// ManagedBy and TokenLabel are the sandbox backend's label contract
	// (internal/sandbox/kubernetes).
	ManagedBy  string
	TokenLabel string
	ProxyPort  int32 // sandbox pod data port (proxy.DefaultProxyPort)
	AdminPort  int32 // sandbox pod admin port for direct probing (proxy.DefaultAdminPort)
}

// SandboxProxy is the sandbox data plane: it resolves the capability token from
// the request's Host and proxies to the workload carrying that token.
//
// Sync only. Async delivery is deployment-typed (its callback and event triple
// come off a deployment spec), and a sandbox has no need for it: async
// execution belongs to the image's own /execute contract, not to us. Staying
// sync also keeps file transfers streaming rather than buffered.
type SandboxProxy struct {
	targets SandboxTargets
	broker  *broker
	addr    sandbox.Addressing
	cfg     SandboxConfig
}

// NewSandboxProxy creates a SandboxProxy over a target resolver. rec (nilable)
// receives the hold metrics.
func NewSandboxProxy(targets SandboxTargets, cfg SandboxConfig, rec Recorder) *SandboxProxy {
	if cfg.Hold <= 0 {
		cfg.Hold = defaultSandboxHold
	}
	return &SandboxProxy{
		targets: targets,
		broker:  newBroker(rec, componentSandboxProxy),
		addr:    sandbox.Addressing{Domain: cfg.Domain},
		cfg:     cfg,
	}
}

// PodTargets resolves sandbox tokens from a Kubernetes pod informer.
type PodTargets struct {
	client kubernetes.Interface
	cfg    PodTargetsConfig
	pods   corelisters.PodLister
}

// NewPodTargets creates the Kubernetes target resolver. Call Start before
// serving.
func NewPodTargets(client kubernetes.Interface, cfg PodTargetsConfig) *PodTargets {
	if cfg.ProxyPort == 0 {
		cfg.ProxyPort = proxy.DefaultProxyPort
	}
	if cfg.AdminPort == 0 {
		cfg.AdminPort = proxy.DefaultAdminPort
	}
	return &PodTargets{client: client, cfg: cfg}
}

// Start runs the sandbox pod informer on ctx and blocks until its cache syncs;
// the informer keeps running until ctx cancels.
func (t *PodTargets) Start(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(t.client, sandboxInformerResync,
		informers.WithNamespace(t.cfg.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = revisionLabelManagedBy + "=" + t.cfg.ManagedBy + "," + t.cfg.TokenLabel
		}),
	)
	pods := factory.Core().V1().Pods()
	t.pods = pods.Lister()

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pods.Informer().HasSynced) {
		return errors.New("informer caches failed to sync")
	}
	return nil
}

// Target returns the data port of the pod carrying this token — a ready one, or
// during creation the first whose sidecar answers a direct /ready probe (ahead
// of kubelet readiness propagation, which is most of a sub-second claim).
func (t *PodTargets) Target(ctx context.Context, token string) (*url.URL, error) {
	selector := labels.SelectorFromSet(labels.Set{t.cfg.TokenLabel: token})
	pods, err := t.pods.Pods(t.cfg.Namespace).List(selector)
	if err != nil {
		return nil, err
	}
	if target := readyPodTarget(pods, t.cfg.ProxyPort); target != nil {
		return target, nil
	}
	return probeCandidates(ctx, pods, t.cfg.ProxyPort, t.cfg.AdminPort), nil
}

// ServeHTTP implements the sandbox data plane. The token is read from the Host
// and never logged or echoed: it is the credential, so an error body says only
// that nothing answers there.
func (a *SandboxProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, port, ok := a.addr.Resolve(r.Host)
	if !ok {
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
	a.broker.sync(w, r, token, r.Host, a.cfg.Hold, sandboxCapacity{targets: a.targets, token: token})
}

// Matches reports whether this host addresses a sandbox. Used where one listener
// serves both data planes (the Docker backend) to pick between them — so a host
// that merely shares the domain must NOT match, which is why the grammar
// requires the sandbox prefix rather than trimming it.
func (a *SandboxProxy) Matches(host string) bool {
	_, _, ok := a.addr.Resolve(host)
	return ok
}

// sandboxCapacity adapts one sandbox to the broker's seam: the target is
// whatever serves this token. It implements no riser — a sandbox is a claimed
// workload, so if it is gone, it is gone, and the broker's only job here is the
// one legitimate wait: the tail of the sandbox's own creation.
type sandboxCapacity struct {
	targets SandboxTargets
	token   string
}

func (c sandboxCapacity) Target(ctx context.Context) (*url.URL, error) {
	return c.targets.Target(ctx, c.token)
}

