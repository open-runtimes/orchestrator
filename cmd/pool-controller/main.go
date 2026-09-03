// pool-controller owns bare warm-pod inventory for every orchestrator
// consumer. POOL_KIND selects the consumer-specific pod contract; lifecycle
// after a pod is claimed remains with that consumer's service.
package main

import (
	"context"
	"errors"
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
	"orchestrator/internal/warm"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
)

const defaultLeaseName = "pool-controller-leader"

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
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := metrics.Shutdown(shutdownCtx); err != nil {
			slog.Warn("Metrics shutdown failed", "error", err)
		}
	}()
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
		Enabled:       config.GetEnv("KUBE_POOL_LEADER_ELECTION", "") == "true",
		LeaseName:     config.GetEnv("KUBE_POOL_LEADER_LEASE_NAME", defaultLeaseName),
		Identity:      config.GetEnv("KUBE_POOL_LEADER_IDENTITY", ""),
		LeaseDuration: config.GetDurationEnv("KUBE_POOL_LEADER_LEASE_DURATION", 15*time.Second),
		RenewDeadline: config.GetDurationEnv("KUBE_POOL_LEADER_RENEW_DEADLINE", 10*time.Second),
		RetryPeriod:   config.GetDurationEnv("KUBE_POOL_LEADER_RETRY_PERIOD", 2*time.Second),
	}
	if leader.Enabled {
		leader.ApplyDefaults(defaultLeaseName)
	}
	go kube.RunLeaderElected(ctx, client, namespace, leader,
		func(termCtx context.Context) { manager.RunControl(termCtx, warm.Hooks{}) },
		func(eventCtx context.Context, identity string, leading bool) {
			metrics.RecordLeadership(eventCtx, identity, leading)
		})
	slog.Info("Pool controller ready", "kind", kind, "namespace", namespace, "pools", poolCount)
	return serveHealth(ctx)
}

func buildManager(client kubernetes.Interface, metrics *observability.Metrics, kind string) (*warm.Manager, int, string, error) {
	switch kind {
	case "revision":
		pools, err := pool.LoadPools(config.GetEnv("POOLS_JSON", ""))
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

func serveHealth(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: ":" + config.GetEnv("HEALTH_PORT", "8080"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return fmt.Errorf("health server: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("Metrics server shutdown failed", "error", err)
	}
	return nil
}
