// deployments-activator is the Kubernetes buffering data plane for deployments:
// the gateway routes cold-start and async traffic here, tagged X-Revision per
// weighted backendRef, and it holds each request until the revision serves —
// raising it from zero when needed. Warm sync traffic bypasses it entirely.
//
// Sandboxes have their own data plane (cmd/sandbox-proxy) rather than a mode of
// this one: it is permanently on the request path, needs no scale or Secret
// access, and has nothing to raise. See docs/deployments.md.
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
	"orchestrator/pkg/server"
	"os"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "deployments-activator"))

	if err := run(context.Background()); err != nil {
		slog.Error("Activator failed", "error", err)
		os.Exit(1)
	}
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
