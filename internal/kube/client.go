// Package kube provides the Kubernetes plumbing shared by orchestrator
// backends: client construction, API-latency instrumentation, and
// lease-based leader election.
package kube

import (
	"fmt"
	"orchestrator/internal/observability"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

// NewConfig resolves the rest config every client of one process is built
// from. When metrics is non-nil, all API requests are instrumented with
// latency/error recorders. A positive qps installs one shared token bucket,
// so typed, dynamic, and Gateway clients derived from the same config draw on
// a single budget rather than one bucket each; zero retains client-go
// defaults.
func NewConfig(kubeconfig, kubeContext string, metrics *observability.Metrics, qps float32, burst int) (*rest.Config, error) {
	restCfg, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, fmt.Errorf("failed to build kube config: %w", err)
	}
	if metrics != nil {
		restCfg.Wrap(newMetricsTransport(metrics))
	}
	if qps > 0 {
		restCfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(qps, max(burst, 1))
	}
	return restCfg, nil
}

// NewClient builds a Kubernetes client with client-go's default rate limits.
func NewClient(kubeconfig, kubeContext string, metrics *observability.Metrics) (*kubernetes.Clientset, error) {
	restCfg, err := NewConfig(kubeconfig, kubeContext, metrics, 0, 0)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	return cs, nil
}

// buildRestConfig resolves a *rest.Config in this order:
//  1. in-cluster config (when running as a pod) — only when neither kubeconfig
//     nor context is explicitly requested, so explicit overrides win;
//  2. an explicit kubeconfig path with optional context override;
//  3. default kubeconfig loading rules ($KUBECONFIG or $HOME/.kube/config)
//     with optional context override.
func buildRestConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	if kubeconfig == "" && kubeContext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
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
