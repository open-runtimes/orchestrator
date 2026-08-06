// Package activator is the deployments data-plane edge for Phase 1: it is
// always on the request path, routing by Host to a deployment's ready proxy
// endpoint, and owning the sync/async split (Prefer: respond-async → 202 +
// callback). From Phase 3 the gateway takes the warm path and this component
// only buffers cold/async traffic. See docs/deployments.md.
package activator

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"orchestrator/internal/deployment"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/workload"
	"strings"
	"sync"
	"time"
)

// resolveTTL bounds how long a host→spec resolution is reused on the data
// path. Spec changes (or deletes) take up to this long to be seen here.
const resolveTTL = time.Second

// Resolver maps a request host to its deployment spec, supplies ready
// endpoints, and scales capacity. Implemented by deployment.Service.
type Resolver interface {
	Resolve(ctx context.Context, host string) (*deployment.Request, error)
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)
	Scale(ctx context.Context, id string, replicas int) error
}

// Activator routes data-plane traffic by Host.
type Activator struct {
	resolver Resolver
	broker   *deploymentBroker

	mu    sync.Mutex
	cache map[string]resolveEntry // host → spec, TTL-bounded
}

type resolveEntry struct {
	spec    *deployment.Request
	expires time.Time
}

// New creates an Activator. queue delivers async response callbacks; rec
// (nilable) receives the hold/raise/async metrics.
func New(resolver Resolver, queue dispatcher.Queue, rec Recorder) *Activator {
	return &Activator{
		resolver: resolver,
		broker:   newDeploymentBroker(queue, rec),
		cache:    make(map[string]resolveEntry),
	}
}

// QueuedDepth reports how many requests are currently waiting for the
// deployment's first endpoint — the autoscaler's hold-up signal during a
// cold start.
func (a *Activator) QueuedDepth(id string) int {
	return a.broker.queuedDepth(id)
}

// resolve is the cached host→spec lookup for the data path; misses fall
// through to the Resolver (a full backend scan).
func (a *Activator) resolve(ctx context.Context, host string) (*deployment.Request, error) {
	a.mu.Lock()
	entry, ok := a.cache[host]
	a.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.spec, nil
	}

	spec, err := a.resolver.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cache[host] = resolveEntry{spec: spec, expires: time.Now().Add(resolveTTL)}
	pruneCache(a.cache)
	a.mu.Unlock()
	return spec, nil
}

// ServeHTTP implements the data plane: resolve Host → deployment, then hand
// the request to the broker with this deployment's capacity bound in.
func (a *Activator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	spec, err := a.resolve(r.Context(), host)
	if err != nil {
		http.Error(w, "no deployment for host "+host, http.StatusNotFound)
		return
	}

	c := deploymentCapacity{resolver: a.resolver, spec: spec}
	hold := time.Duration(spec.StartTimeoutSeconds) * time.Second

	// Forward the host the client actually used — any of the deployment's
	// hosts is a valid virtual host for the workload.
	if workload.PreferAsync(r) {
		a.broker.async(w, r, spec.ID, host, spec, hold, c)
		return
	}
	a.broker.sync(w, r, spec.ID, host, hold, c)
}

// deploymentCapacity adapts one resolved deployment to the broker's seam:
// targets are the Resolver's ready endpoints, a raise scales the deployment
// to its declared replica count.
type deploymentCapacity struct {
	resolver Resolver
	spec     *deployment.Request
}

func (c deploymentCapacity) Target(ctx context.Context) (*url.URL, error) {
	endpoints, err := c.resolver.Endpoints(ctx, c.spec.ID)
	if err != nil || len(endpoints) == 0 {
		return nil, err
	}
	// Spread load across replicas — always taking the first would pin all
	// activator traffic to whichever endpoint lists first.
	return endpoints[rand.IntN(len(endpoints))], nil
}

func (c deploymentCapacity) Raise(ctx context.Context) error {
	replicas := max(c.spec.Replicas, 1)
	if err := c.resolver.Scale(ctx, c.spec.ID, replicas); err != nil {
		return err
	}
	slog.Info("Cold-start scale-up requested", "deploymentId", c.spec.ID, "replicas", replicas)
	return nil
}

func hostOnly(hostport string) string {
	if i := strings.LastIndex(hostport, ":"); i != -1 && !strings.Contains(hostport[i:], "]") {
		return hostport[:i]
	}
	return hostport
}

// pruneCache drops expired resolve entries.
func pruneCache(m map[string]resolveEntry) {
	if len(m) < pruneMapThreshold {
		return
	}
	now := time.Now()
	for k, e := range m {
		if now.After(e.expires) {
			delete(m, k)
		}
	}
}
