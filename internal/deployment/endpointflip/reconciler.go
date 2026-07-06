// Package endpointflip reconciles the SKS-style flip EndpointSlice for every
// managed revision Service: endpoints are the revision's ready pods when warm
// and the shared activator pods when cold or draining. Route backendRefs never
// change — only endpoint membership does (docs/design/gateway-routing.md,
// docs/design/deployments-activator.md).
package endpointflip

import (
	"context"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Labels mirroring the deployments-service convention (see
// internal/deployment/kubernetes/mapper.go). LabelRevision is new in Phase 3:
// it carries the revision name (e.g. "web-00001") and marks a Service as a
// selectorless revision Service whose endpoints this package owns.
const (
	LabelManagedBy    = "managed-by"
	LabelDeploymentID = "deployment.id"
	LabelRevision     = "deployment.revision"
	ManagedByValue    = "deployments-service"
)

const (
	sliceSuffix   = "-flip"
	portName      = "http"
	defaultResync = 30 * time.Second
)

// Options configures a Reconciler.
type Options struct {
	// ActivatorSelector selects the activator pods that back cold revisions,
	// e.g. "app.kubernetes.io/component=deployments-activator". Empty or
	// unparsable means cold revisions flip to a slice with zero endpoints.
	ActivatorSelector string
	// ActivatorNamespace is where the activator pods run when it differs from
	// the reconciler's namespace (control plane vs. hardened workload
	// namespace). Empty means the reconciler's own namespace.
	ActivatorNamespace string
	// ProxyPort is the endpoint target port for warm (revision pod) mode.
	ProxyPort int32
	// ActivatorPort is the endpoint target port for activator mode.
	ActivatorPort int32
	// Resync is the informer resync period; defaults to 30s.
	Resync time.Duration
}

// Reconciler owns the {service}-flip EndpointSlice of every managed revision
// Service in one namespace.
type Reconciler struct {
	client             kubernetes.Interface
	namespace          string
	activatorNamespace string
	opts               Options

	// nil when Options.ActivatorSelector is unusable; the flip then degrades
	// to empty endpoints rather than matching every pod.
	activatorSelector labels.Selector

	queue workqueue.TypedRateLimitingInterface[string]

	// services maps pod events to affected Services; set in Run before the
	// informers start. Reconcile reads go through the client instead, so a
	// reconcile always converges on the authoritative state.
	services listersv1.ServiceNamespaceLister
}

// New builds a Reconciler for one namespace. Call Run to start it.
func New(client kubernetes.Interface, namespace string, opts Options) *Reconciler {
	if opts.Resync <= 0 {
		opts.Resync = defaultResync
	}
	r := &Reconciler{
		client:             client,
		namespace:          namespace,
		activatorNamespace: opts.ActivatorNamespace,
		opts:               opts,
		queue:              workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	if r.activatorNamespace == "" {
		r.activatorNamespace = namespace
	}
	// labels.Parse("") yields Everything — reject it too, or every pod in the
	// namespace would count as an activator.
	if sel, err := labels.Parse(opts.ActivatorSelector); err != nil || sel.Empty() {
		slog.Warn("No usable activator selector; cold revisions get empty endpoint slices",
			"selector", opts.ActivatorSelector, "error", err)
	} else {
		r.activatorSelector = sel
	}
	return r
}

// Run starts the informers and the reconcile loop, blocking until ctx cancels.
func (r *Reconciler) Run(ctx context.Context) {
	serviceFactory := informers.NewSharedInformerFactoryWithOptions(r.client, r.opts.Resync,
		informers.WithNamespace(r.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = LabelManagedBy + "=" + ManagedByValue
		}),
	)
	// Pods cannot share the Service filter: revision pods and activator pods
	// carry disjoint label sets.
	podFactory := informers.NewSharedInformerFactoryWithOptions(r.client, r.opts.Resync,
		informers.WithNamespace(r.namespace))

	serviceInformer := serviceFactory.Core().V1().Services()
	r.services = serviceInformer.Lister().Services(r.namespace)
	_, _ = serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    r.enqueueService,
		UpdateFunc: func(_, obj any) { r.enqueueService(obj) },
		DeleteFunc: r.enqueueService,
	})
	_, _ = podFactory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    r.enqueueForPod,
		UpdateFunc: func(_, obj any) { r.enqueueForPod(obj) },
		DeleteFunc: r.enqueueForPod,
	})

	// Activator pods live with the control plane, which may be a different
	// namespace than the workloads; the local pod informer cannot see them.
	var activatorFactory informers.SharedInformerFactory
	if r.activatorSelector != nil && r.activatorNamespace != r.namespace {
		activatorFactory = informers.NewSharedInformerFactoryWithOptions(r.client, r.opts.Resync,
			informers.WithNamespace(r.activatorNamespace),
			informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
				opts.LabelSelector = r.activatorSelector.String()
			}),
		)
		enqueueAll := func(any) { r.enqueueRevisionServices(labels.Everything()) }
		_, _ = activatorFactory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    enqueueAll,
			UpdateFunc: func(_, obj any) { enqueueAll(obj) },
			DeleteFunc: enqueueAll,
		})
	}

	serviceFactory.Start(ctx.Done())
	podFactory.Start(ctx.Done())
	serviceFactory.WaitForCacheSync(ctx.Done())
	podFactory.WaitForCacheSync(ctx.Done())
	if activatorFactory != nil {
		activatorFactory.Start(ctx.Done())
		activatorFactory.WaitForCacheSync(ctx.Done())
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.processQueue(ctx)
	}()
	<-ctx.Done()
	r.queue.ShutDown()
	<-done
}

func (r *Reconciler) processQueue(ctx context.Context) {
	for {
		name, shutdown := r.queue.Get()
		if shutdown {
			return
		}
		if err := r.reconcileService(ctx, name); err != nil {
			slog.Warn("Endpoint flip reconcile failed", "service", name, "error", err)
			r.queue.AddRateLimited(name)
		} else {
			r.queue.Forget(name)
		}
		r.queue.Done(name)
	}
}

func (r *Reconciler) enqueueService(obj any) {
	svc := serviceFrom(obj)
	if svc == nil || svc.Labels[LabelRevision] == "" {
		return
	}
	r.queue.Add(svc.Name)
}

// enqueueForPod maps a pod event to the Services it can flip: revision pods
// affect the Services sharing their revision label; activator pods affect
// every managed revision Service (they back any cold one).
func (r *Reconciler) enqueueForPod(obj any) {
	pod := podFrom(obj)
	if pod == nil {
		return
	}
	if rev := pod.Labels[LabelRevision]; rev != "" {
		r.enqueueRevisionServices(labels.Set{LabelRevision: rev}.AsSelector())
	}
	if r.activatorSelector != nil && r.activatorSelector.Matches(labels.Set(pod.Labels)) {
		r.enqueueRevisionServices(labels.Everything())
	}
}

func (r *Reconciler) enqueueRevisionServices(sel labels.Selector) {
	services, err := r.services.List(sel)
	if err != nil {
		return
	}
	for _, svc := range services {
		if svc.Labels[LabelRevision] != "" {
			r.queue.Add(svc.Name)
		}
	}
}

// podFrom / serviceFrom unwrap informer event payloads, including deletion
// tombstones.
func podFrom(obj any) *corev1.Pod {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	pod, _ := obj.(*corev1.Pod)
	return pod
}

func serviceFrom(obj any) *corev1.Service {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	svc, _ := obj.(*corev1.Service)
	return svc
}
