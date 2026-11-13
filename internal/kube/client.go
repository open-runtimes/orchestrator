// Package kube provides the Kubernetes plumbing shared by orchestrator
// backends: client construction, API-latency instrumentation, and
// lease-based leader election.
package kube

import (
	"fmt"
	"orchestrator/internal/observability"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

// DefaultClientQPS and DefaultClientBurst are the client-side API budget a
// process gets when it does not configure one.
const (
	DefaultClientQPS   = 200
	DefaultClientBurst = 400
)

// NewConfig resolves the rest config every client of one process is built
// from. When metrics is non-nil, all API requests are instrumented with
// latency/error recorders. It always installs one shared token bucket, so
// typed, dynamic, and Gateway clients derived from the same config draw on a
// single budget rather than one bucket each; a qps or burst of zero or less
// takes DefaultClientQPS or DefaultClientBurst.
func NewConfig(kubeconfig, kubeContext string, metrics *observability.Metrics, qps float32, burst int) (*rest.Config, error) {
	restConfig, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, fmt.Errorf("failed to build kube config: %w", err)
	}
	if metrics != nil {
		restConfig.Wrap(newMetricsTransport(metrics))
	}
	if qps <= 0 {
		qps = DefaultClientQPS
	}
	if burst <= 0 {
		burst = DefaultClientBurst
	}
	restConfig.RateLimiter = deadlineLimiter{flowcontrol.NewTokenBucketRateLimiter(qps, burst)}
	return restConfig, nil
}

// buildRestConfig resolves a *rest.Config in this order:
//  1. in-cluster config (when running as a pod) — only when neither kubeconfig
//     nor context is explicitly requested, so explicit overrides win;
//  2. an explicit kubeconfig path with optional context override;
//  3. default kubeconfig loading rules ($KUBECONFIG or $HOME/.kube/config)
//     with optional context override.
func buildRestConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	if kubeconfig == "" && kubeContext == "" {
		if config, err := rest.InClusterConfig(); err == nil {
			return config, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}
