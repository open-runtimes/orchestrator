// job-sidecar runs alongside job containers to handle input downloads and output processing.
package main

import (
	"context"
	"log/slog"
	"orchestrator/internal/sidecar"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// Check if ready (used by Docker health checks)
	// Exits 0 if marker file exists, 1 otherwise
	if len(os.Args) > 1 && os.Args[1] == "-check-ready" {
		path := os.Getenv("SHARED_VOLUME_PATH")
		if path == "" {
			path = "/workspace"
		}
		if sidecar.CheckReady(path) {
			os.Exit(0)
		}
		os.Exit(1)
	}

	// Setup structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("Sidecar failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Load configuration
	cfg := sidecar.LoadConfigFromEnv()

	if cfg.JobID == "" {
		slog.Error("JOB_ID environment variable is required")
		return nil // Exit cleanly to avoid double error message
	}

	// Create runner
	runner, err := sidecar.NewRunner(cfg)
	if err != nil {
		return err
	}
	defer runner.Close()

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	// Run the sidecar (logs completion internally)
	return runner.Run(ctx)
}
