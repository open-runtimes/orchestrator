// job-sidecar runs alongside job containers to handle input downloads and output processing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sidecar"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	var (
		checkReady  bool
		checkMounts bool
		mode        string
	)
	flag.BoolVar(&checkReady, "check-ready", false, "exit 0 if the pre-job ready marker exists, 1 otherwise")
	flag.BoolVar(&checkMounts, "check-mounts", false, "exit 0 if the mounts-ready marker exists, 1 otherwise")
	flag.StringVar(&mode, "mode", "combined", "sidecar mode: combined (Docker), pre (K8s init container), post (K8s native sidecar)")
	flag.Parse()

	// Probe paths — must stay silent (no log setup) to avoid polluting status output.
	if checkReady || checkMounts {
		path := os.Getenv("SHARED_VOLUME_PATH")
		if path == "" {
			path = "/workspace"
		}
		ready := sidecar.CheckReady(path)
		if checkMounts {
			ready = sidecar.CheckMountsReady(path)
		}
		if ready {
			os.Exit(0)
		}
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(mode); err != nil {
		slog.Error("Sidecar failed", "error", err, "mode", mode)
		os.Exit(1)
	}
}

func run(mode string) error {
	cfg := sidecar.LoadConfigFromEnv()

	if cfg.JobID == "" {
		slog.Error("JOB_ID environment variable is required")
		return nil
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
		sidecar.WithS3Credentials(cfg.S3),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel the outer context on SIGINT/SIGTERM so pre-artifact processing
	// can abort cleanly. The Runner's waitFn registers its own SIGUSR1/SIGTERM
	// handlers during waits; in post mode, post-artifact processing runs on a
	// detached context so this cancellation does not short-circuit it.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	switch mode {
	case "pre":
		return runner.RunPre(ctx, artifacts)
	case "post":
		return runner.RunPost(ctx, artifacts)
	case "combined":
		return runner.Run(ctx, artifacts)
	default:
		return fmt.Errorf("invalid mode %q (expected combined|pre|post)", mode)
	}
}
