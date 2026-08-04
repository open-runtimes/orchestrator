// deployments-activator is the Kubernetes data-plane edge. It runs in one of
// two modes, same binary and image, separate Deployments:
//
//   - EDGE_MODE=deployment (default): the buffering edge for deployments. The
//     gateway routes cold-start and async traffic here, tagged X-Revision per
//     weighted backendRef; warm sync traffic bypasses it entirely.
//   - EDGE_MODE=sandbox: the sandbox edge. One wildcard route sends every
//     sandbox here and it resolves the sandbox from the request's Host. Unlike
//     the deployment edge it is permanently on the data path, which is exactly
//     why it gets its own replica set: sandbox file transfers have no business
//     sharing a failure domain with deployment cold starts.
//
// See docs/deployments.md and docs/sandboxes.md.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/activator"
	"orchestrator/internal/config"
	"orchestrator/internal/dispatcher"
	"orchestrator/internal/kube"
	"orchestrator/internal/observability"
	sandboxkubernetes "orchestrator/internal/sandbox/kubernetes"
	"orchestrator/pkg/server"
	"os"
	"time"
)

func main() {
	mode := config.GetEnv("EDGE_MODE", modeDeployment)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "deployments-activator", "mode", mode))

	serve := run
	if mode == modeSandbox {
		serve = runSandbox
	}
	if err := serve(context.Background()); err != nil {
		slog.Error("Activator failed", "error", err)
		os.Exit(1)
	}
}

// Edge modes.
const (
	modeDeployment = "deployment"
	modeSandbox    = "sandbox"
)

// runSandbox serves the sandbox edge until SIGINT/SIGTERM. There is no
// dispatcher and no /stats: sandbox traffic is sync only (async execution is
// the sandbox image's own contract), and a sandbox has no scale-from-zero for
// an autoscaler to hold up on.
func runSandbox(ctx context.Context) error {
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		return fmt.Errorf("failed to init metrics: %w", err)
	}

	client, err := kube.NewClient(config.GetEnv("KUBECONFIG", ""), config.GetEnv("KUBE_CONTEXT", ""), metrics)
	if err != nil {
		return err
	}

	edge := activator.NewSandboxActivator(client, activator.SandboxConfig{
		Namespace:  config.GetEnv("KUBE_NAMESPACE", "orchestrator"),
		Domain:     config.GetEnv("SANDBOX_DOMAIN", "localhost"),
		ManagedBy:  sandboxkubernetes.ManagedByValue,
		TokenLabel: sandboxkubernetes.LabelToken,
		Hold:       time.Duration(config.GetIntEnv("SANDBOX_HOLD_SECONDS", 5)) * time.Second,
	}, metrics)
	if err := edge.Start(ctx); err != nil {
		return err
	}
	slog.Info("Informer caches synced")

	// The data listener serves the edge and NOTHING else. Every path belongs to
	// the sandbox: mounting our own /healthz here would shadow the contract's
	// /healthz for every sandbox behind the edge. Our probes live on the
	// management port.
	dataServer := &http.Server{
		Addr:              ":" + config.GetEnv("ACTIVATOR_PORT", "8081"),
		Handler:           edge,
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

// run serves the activator data plane until SIGINT/SIGTERM.
func run(ctx context.Context) error {
	metrics, metricsHandler, err := observability.NewMetrics(ctx)
	if err != nil {
		return fmt.Errorf("failed to init metrics: %w", err)
	}

	client, err := kube.NewClient(config.GetEnv("KUBECONFIG", ""), config.GetEnv("KUBE_CONTEXT", ""), metrics)
	if err != nil {
		return err
	}

	queue := dispatcher.NewMemory(dispatcher.LoadConfigFromEnv(), metrics)
	act := activator.NewRevisionActivator(client, queue, activator.RevisionConfig{
		Namespace:    config.GetEnv("KUBE_NAMESPACE", "orchestrator"),
		StartTimeout: time.Duration(config.GetIntEnv("START_TIMEOUT_SECONDS", 300)) * time.Second,
	}, metrics)
	if err := act.Start(ctx); err != nil {
		return err
	}
	slog.Info("Informer caches synced")

	// /healthz backs this Deployment's own probes; it is wired up only after
	// Start returns, so ready implies synced informer caches. Everything else
	// is the data plane.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// The autoscaler scrapes queued-per-revision as its cold-start hold-up
	// signal (there are no sidecars to scrape while a revision is at zero).
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"queued": act.QueuedByRevision()})
	})
	mux.Handle("/", act)

	// The data plane goes on an Extra server: a buffered cold start
	// legitimately holds the response up to StartTimeout, far past
	// the management server's WriteTimeout.
	dataServer := &http.Server{
		Addr:              ":" + config.GetEnv("ACTIVATOR_PORT", "8081"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The management port serves only /healthz (and keeps server.Serve's
	// probe-friendly defaults).
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
		Cleanup: func(cleanupCtx context.Context) {
			slog.Info("Draining callback dispatcher")
			if err := queue.Close(cleanupCtx); err != nil {
				slog.Warn("Dispatcher shutdown error", "error", err)
			}
		},
	})
}
