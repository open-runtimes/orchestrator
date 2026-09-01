// Package kube provides the Kubernetes plumbing shared by orchestrator
// backends: client construction, API-latency instrumentation, and
// lease-based leader election.
package kube

import (
	"fmt"
	"orchestrator/internal/observability"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

// NewClient builds a Kubernetes client. When metrics is non-nil, all API
// requests are instrumented with latency/error recorders.
func NewClient(kubeconfig, kubeContext string, metrics *observability.Metrics) (*kubernetes.Clientset, error) {
	return NewClientWithRate(kubeconfig, kubeContext, metrics, 0, 0)
}

// NewClientWithRate builds a Kubernetes client with an explicit client-side
// rate budget. Zero values retain client-go defaults.
func NewClientWithRate(kubeconfig, kubeContext string, metrics *observability.Metrics, qps float32, burst int) (*kubernetes.Clientset, error) {
	restCfg, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, fmt.Errorf("failed to build kube config: %w", err)
	}
	if metrics != nil {
		restCfg.Wrap(newMetricsTransport(metrics))
	}
	applyClientRate(restCfg, qps, burst)
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	return cs, nil
}

// NewDynamicClient builds the client used for orchestrator CRDs.
func NewDynamicClient(kubeconfig, kubeContext string, metrics *observability.Metrics) (*dynamic.DynamicClient, error) {
	return NewDynamicClientWithRate(kubeconfig, kubeContext, metrics, 0, 0)
}

// NewDynamicClientWithRate is NewDynamicClient with an explicit client-side
// rate budget. The typed and dynamic clients have independent token buckets.
func NewDynamicClientWithRate(kubeconfig, kubeContext string, metrics *observability.Metrics, qps float32, burst int) (*dynamic.DynamicClient, error) {
	restCfg, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, fmt.Errorf("failed to build kube config: %w", err)
	}
	if metrics != nil {
		restCfg.Wrap(newMetricsTransport(metrics))
	}
	applyClientRate(restCfg, qps, burst)
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic kube client: %w", err)
	}
	return client, nil
}

func applyClientRate(cfg *rest.Config, qps float32, burst int) {
	if qps > 0 {
		cfg.QPS = qps
	}
	if burst > 0 {
		cfg.Burst = burst
	}
}

// NewGatewayClient builds a Gateway API client (HTTPRoute reconciliation),
// resolving its rest config exactly like NewClient.
func NewGatewayClient(kubeconfig, kubeContext string) (*gatewayclient.Clientset, error) {
	return NewGatewayClientWithRate(kubeconfig, kubeContext, 0, 0)
}

// NewGatewayClientWithRate builds the Gateway API client with an explicit
// rate budget, matching the typed and dynamic clients during deployment
// bursts that also create or update HTTPRoutes.
func NewGatewayClientWithRate(kubeconfig, kubeContext string, qps float32, burst int) (*gatewayclient.Clientset, error) {
	restCfg, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, fmt.Errorf("failed to build kube config: %w", err)
	}
	applyClientRate(restCfg, qps, burst)
	cs, err := gatewayclient.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create gateway client: %w", err)
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
