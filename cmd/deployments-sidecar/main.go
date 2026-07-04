// deployments-sidecar is the reverse proxy fronting the user container in
// every deployment replica: readiness gating, graceful drain, per-request
// timeout, and the hard concurrency cap. See docs/design/deployments-sidecar.md.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"orchestrator/internal/proxy"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	var checkReady bool
	flag.BoolVar(&checkReady, "check-ready", false, "exit 0 if the proxy reports ready, 1 otherwise")
	flag.Parse()

	// Probe path — must stay silent (no log setup) to avoid polluting status output.
	if checkReady {
		if ready() {
			os.Exit(0)
		}
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "deployments-sidecar"))

	if err := run(); err != nil {
		slog.Error("Sidecar failed", "error", err)
		os.Exit(1)
	}
}

// run serves the proxy until SIGINT/SIGTERM, then drains.
func run() error {
	cfg := proxy.LoadConfigFromEnv()
	if cfg.Target == "" {
		return errors.New(proxy.EnvTarget + " environment variable is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	return proxy.New(cfg).Run(ctx)
}

// ready probes the local admin /ready endpoint.
func ready() bool {
	port := os.Getenv(proxy.EnvAdminPort)
	if port == "" {
		port = strconv.Itoa(proxy.DefaultAdminPort)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/ready", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
