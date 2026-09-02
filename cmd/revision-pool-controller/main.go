// revision-pool-controller owns the warm inventory used by deployment
// Revisions. The deployments service only claims and binds pods from this
// inventory; replenishment and garbage collection live here.
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
	"orchestrator/internal/warm"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
)

const defaultLeaseName = "revision-pool-controller-leader"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "revision-pool-controller"))
	if err := run(ctx); err != nil {
		slog.Error("Revision pool controller failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	pools, err := pool.LoadPools(config.GetEnv("POOLS_JSON", ""))
	if err != nil {
		return fmt.Errorf("invalid pool configuration: %w", err)
	}
	cfg, err := depkubernetes.LoadConfigFromEnv()
	if err != nil {
		return fmt.Errorf("invalid Kubernetes configuration: %w", err)
	}
	cfg.SidecarImage = config.GetEnv("WORKLOAD_SIDECAR_IMAGE", "workload-sidecar:latest")
	cfg.PoolShimImage = config.GetEnv("POOL_SHIM_IMAGE", "pool-shim:latest")
	cfg.Pools = pools
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		return fmt.Errorf("initialize metrics: %w", err)
	}
	cfg.Metrics = metrics
	cfg.LeaderElection = kube.LeaderElectionConfig{
		Enabled:       config.GetEnv("KUBE_POOL_LEADER_ELECTION", "") == "true",
		LeaseName:     config.GetEnv("KUBE_POOL_LEADER_LEASE_NAME", defaultLeaseName),
		Identity:      config.GetEnv("KUBE_POOL_LEADER_IDENTITY", ""),
		LeaseDuration: config.GetDurationEnv("KUBE_POOL_LEADER_LEASE_DURATION", 15*time.Second),
		RenewDeadline: config.GetDurationEnv("KUBE_POOL_LEADER_RENEW_DEADLINE", 10*time.Second),
		RetryPeriod:   config.GetDurationEnv("KUBE_POOL_LEADER_RETRY_PERIOD", 2*time.Second),
	}
	if cfg.LeaderElection.Enabled {
		cfg.LeaderElection.ApplyDefaults(defaultLeaseName)
	}

	restCfg, err := kube.NewConfig(cfg.Kubeconfig, cfg.Context, nil, float32(cfg.ClientQPS), cfg.ClientBurst)
	if err != nil {
		return fmt.Errorf("build Kubernetes configuration: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	manager, err := depkubernetes.NewRevisionPoolManager(client, cfg)
	if err != nil {
		return fmt.Errorf("invalid Revision pool configuration: %w", err)
	}
	if err := manager.Verify(ctx); err != nil {
		return fmt.Errorf("verify Revision pools: %w", err)
	}
	statuses, err := manager.PoolStatuses(ctx)
	if err != nil {
		return fmt.Errorf("survey Revision pools: %w", err)
	}
	for _, status := range statuses {
		slog.Info("Pool reconciled", "pool", status.ID, "size", status.Size, "warm", status.Warm, "claimed", status.Claimed)
	}
	go kube.RunLeaderElected(ctx, client, cfg.Namespace, cfg.LeaderElection,
		func(termCtx context.Context) { manager.RunControl(termCtx, warm.Hooks{}) },
		func(eventCtx context.Context, identity string, leading bool) {
			metrics.RecordLeadership(eventCtx, identity, leading)
		})
	slog.Info("Revision pool controller ready", "namespace", cfg.Namespace, "pools", len(pools))

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metricsHandler)
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: ":" + config.GetEnv("METRICS_PORT", "9090"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-serverErr:
		return fmt.Errorf("metrics server: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("Metrics server shutdown failed", "error", err)
	}
	return nil
}
