// job-sidecar runs alongside job containers to handle input downloads and output processing.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sidecar"
	"os"
	"os/signal"
	"strings"
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
	cfg := sidecar.LoadConfigFromEnv()

	if cfg.JobID == "" {
		slog.Error("JOB_ID environment variable is required")
		return nil // Exit cleanly to avoid double error message
	}

	reg := artifact.DefaultRegistry()
	artifacts, err := reg.Unmarshal([]byte(os.Getenv("ARTIFACTS_JSON")))
	if err != nil {
		return err
	}

	var meta map[string]string
	if cfg.Meta != "" && cfg.Meta != "{}" {
		_ = json.Unmarshal([]byte(cfg.Meta), &meta)
	}

	var callbackEvents []string
	if cfg.CallbackEvents != "" {
		callbackEvents = strings.Split(cfg.CallbackEvents, ",")
	}

	reporter := sidecar.NewHTTPSink(
		cfg.JobID,
		cfg.ArtifactEndpoint,
		cfg.ArtifactTimeout,
		cfg.CallbackURL,
		cfg.CallbackKey,
		callbackEvents,
		meta,
	)

	runner := sidecar.NewRunner(cfg.JobID, cfg.SharedVolumePath, cfg.TimeoutSeconds, reg,
		sidecar.WithArtifactListener(reporter),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	return runner.Run(ctx, artifacts)
}
