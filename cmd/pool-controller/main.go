// pool-controller owns bare warm-pod inventory for every orchestrator
// consumer. POOL_KIND selects the consumer-specific pod contract; lifecycle
// after a pod is claimed remains with that consumer's service.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/config"
	depkubernetes "orchestrator/internal/deployment/kubernetes"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	"orchestrator/internal/pool"
	"orchestrator/internal/sandbox"
	sandboxkubernetes "orchestrator/internal/sandbox/kubernetes"
	"orchestrator/internal/server"
	"orchestrator/internal/warm"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "pool-controller"))
	if err := run(ctx); err != nil {
		slog.Error("Pool controller failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	kind := config.GetEnv("POOL_KIND", "")
	metrics, err := observability.NewMetrics(ctx)
	if err != nil {
		return fmt.Errorf("initialize metrics: %w", err)
	}
	restCfg, err := kube.NewConfig(
		config.GetEnv("KUBECONFIG", ""), config.GetEnv("KUBE_CONTEXT", ""), metrics,
		float32(config.GetIntEnv("KUBE_CLIENT_QPS", 200)), config.GetIntEnv("KUBE_CLIENT_BURST", 400),
	)
	if err != nil {
		return fmt.Errorf("build Kubernetes configuration: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	manager, poolCount, namespace, err := buildManager(client, metrics, kind)
	if err != nil {
		return err
	}
	if err := manager.Verify(ctx); err != nil {
		return fmt.Errorf("verify %s pools: %w", kind, err)
	}
	statuses, err := manager.PoolStatuses(ctx)
	if err != nil {
		return fmt.Errorf("survey %s pools: %w", kind, err)
	}
	for _, status := range statuses {
		slog.Info("Pool reconciled", "kind", kind, "pool", status.ID, "size", status.Size, "warm", status.Warm, "claimed", status.Claimed)
	}

	leader := kube.LeaderElectionConfig{
		Enabled:       config.GetBoolEnv("KUBE_POOL_LEADER_ELECTION", false),
		LeaseName:     config.GetEnv("KUBE_POOL_LEADER_LEASE_NAME", ""),
		Identity:      config.GetEnv("KUBE_POOL_LEADER_IDENTITY", ""),
		LeaseDuration: config.GetDurationEnv("KUBE_POOL_LEADER_LEASE_DURATION", 0),
		RenewDeadline: config.GetDurationEnv("KUBE_POOL_LEADER_RENEW_DEADLINE", 0),
		RetryPeriod:   config.GetDurationEnv("KUBE_POOL_LEADER_RETRY_PERIOD", 0),
	}
	leader.ApplyDefaults("pool-controller-leader")
	go kube.RunLeaderElected(ctx, client, namespace, leader,
		func(termCtx context.Context) { manager.RunControl(termCtx, warm.Hooks{}) },
		metrics.RecordLeadership)
	slog.Info("Pool controller ready", "kind", kind, "namespace", namespace, "pools", poolCount)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return server.Serve(ctx, server.Options{
		Handler:           mux,
		Port:              config.GetEnv("HEALTH_PORT", "8080"),
		TelemetryShutdown: metrics.Shutdown,
	})
}

func buildManager(client kubernetes.Interface, metrics *observability.Metrics, kind string) (*warm.Manager, int, string, error) {
	switch kind {
	case "revision":
		pools, err := pool.Load(config.GetEnv("POOLS_JSON", ""), "POOLS_JSON")
		if err != nil {
			return nil, 0, "", fmt.Errorf("invalid Revision pool configuration: %w", err)
		}
		cfg, err := depkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, 0, "", fmt.Errorf("invalid Revision Kubernetes configuration: %w", err)
		}
		cfg.SidecarImage = config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest")
		cfg.PoolShimImage = config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest")
		cfg.Pools, cfg.Metrics = pools, metrics
		manager, err := depkubernetes.NewRevisionPoolManager(client, cfg)
		return manager, len(pools), cfg.Namespace, err
	case "sandbox":
		pools, err := sandbox.LoadPools(config.GetEnv("SANDBOX_POOLS_JSON", ""))
		if err != nil {
			return nil, 0, "", fmt.Errorf("invalid sandbox pool configuration: %w", err)
		}
		cfg, err := sandboxkubernetes.LoadConfigFromEnv()
		if err != nil {
			return nil, 0, "", fmt.Errorf("invalid sandbox Kubernetes configuration: %w", err)
		}
		cfg.SidecarImage = config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest")
		cfg.ShimImage = config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest")
		cfg.Pools, cfg.Metrics = pools, metrics
		return sandboxkubernetes.NewPoolManager(client, cfg), len(pools), cfg.Namespace, nil
	default:
		return nil, 0, "", fmt.Errorf("unknown POOL_KIND %q (expected revision|sandbox)", kind)
	}
}
