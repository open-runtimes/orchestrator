// sandbox-proxy is the data plane in front of sandboxes: one wildcard route
// (*.{SANDBOX_DOMAIN}) sends every sandbox here, and it resolves which one from
// the capability token in the request's Host.
//
// It is a sibling of deployments-activator, not a mode of it, because the two
// differ in every dimension that matters operationally: this one is permanently
// on the data path (every request to every sandbox, including file transfers)
// while the activator sees only cold and async traffic; this one needs read-only
// access to pods while the activator also writes deployment scales and reads
// spec Secrets; and this one never raises anything, since a sandbox is a claimed
// pod with no zero to rise from. Separate binaries keep those blast radii,
// RBAC grants, and scaling knobs separate — and make it impossible to point a
// Deployment at the wrong data plane.
//
// See docs/sandboxes.md.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/activator"
	"orchestrator/internal/config"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	sandboxkubernetes "orchestrator/internal/sandbox/kubernetes"
	"orchestrator/internal/server"
	"os"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "sandbox-proxy"))

	if err := run(context.Background()); err != nil {
		slog.Error("Sandbox proxy failed", "error", err)
		os.Exit(1)
	}
}

// run serves the sandbox data plane until SIGINT/SIGTERM. There is no
// dispatcher and no /stats: sandbox traffic is sync only (async execution is the
// sandbox image's own contract), and a sandbox has no scale-from-zero for an
// autoscaler to hold up on.
func run(ctx context.Context) error {
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		return fmt.Errorf("failed to init metrics: %w", err)
	}

	client, err := kube.NewClient(config.GetEnv("KUBECONFIG", ""), config.GetEnv("KUBE_CONTEXT", ""), metrics)
	if err != nil {
		return err
	}

	targets := activator.NewPodTargets(client, activator.PodTargetsConfig{
		Namespace:  config.GetEnv("KUBE_NAMESPACE", "orchestrator"),
		ManagedBy:  sandboxkubernetes.ManagedByValue,
		TokenLabel: sandboxkubernetes.LabelToken,
	})
	if err := targets.Start(ctx); err != nil {
		return err
	}
	slog.Info("Informer caches synced")

	sandboxes := activator.NewSandboxProxy(targets, activator.SandboxConfig{
		Domain: config.GetEnv("SANDBOX_DOMAIN", "localhost"),
		Hold:   time.Duration(config.GetIntEnv("SANDBOX_HOLD_SECONDS", 5)) * time.Second,
	}, metrics)

	// The data listener serves sandbox traffic and NOTHING else. Every path
	// belongs to the sandbox: mounting our own /healthz here would shadow the
	// contract's /healthz for every sandbox behind us. Our probes live on the
	// management port.
	dataServer := &http.Server{
		Addr:              ":" + config.GetEnv("ACTIVATOR_PORT", "8081"),
		Handler:           sandboxes,
		ReadHeaderTimeout: 10 * time.Second,
	}

	mgmt := http.NewServeMux()
	mgmt.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return server.Serve(ctx, server.Options{
		Handler:        mgmt,
		MetricsHandler: metricsHandler,
		Port:           config.GetEnv("PORT", "8080"),
		MetricsPort:    config.GetEnv("METRICS_PORT", "9090"),
		Extra:          []*http.Server{dataServer},
	})
}
