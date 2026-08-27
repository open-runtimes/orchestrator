package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
)

// Options configures Serve. Handler and Port are required; the hooks are
// optional.
type Options struct {
	Handler http.Handler
	Port    string

	// Extra servers (e.g. a data-plane listener) started alongside the API
	// server and shut down with it. Addr and Handler must be set.
	Extra []*http.Server

	// DrainWait is how long to keep serving after readiness flips to
	// shutting-down, so load balancers stop routing here (phase 1 of shutdown).
	DrainWait time.Duration
	// SetDraining flips readiness to shutting-down at the start of shutdown.
	SetDraining func()
	// Cleanup runs after the HTTP servers have shut down (phase 3), e.g. to
	// drain a callback dispatcher.
	Cleanup func(context.Context)
	// TelemetryShutdown flushes and stops telemetry after component cleanup.
	TelemetryShutdown func(context.Context) error
}

// Serve runs the API and any extra HTTP servers until SIGINT/SIGTERM or a
// server error, then performs graceful shutdown (drain traffic → shut down
// servers → run cleanup → flush telemetry). It returns nil on a clean shutdown.
func Serve(ctx context.Context, opts Options) error {
	// Route client-go / leaderelection / apimachinery logs (which go through
	// klog) via slog, so everything in the container's stdout is one ndjson
	// stream. Must run before any klog-using library is invoked.
	klog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	apiServer := &http.Server{
		Addr:         ":" + opts.Port,
		Handler:      opts.Handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	serverErr := make(chan error, 1)

	go func() {
		slog.Info("Starting API server", "port", opts.Port)
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	for _, extra := range opts.Extra {
		go func() {
			slog.Info("Starting server", "addr", extra.Addr)
			if err := extra.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}()
	}

	shutdown := func(timeout time.Duration) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		for _, srv := range append([]*http.Server{apiServer}, opts.Extra...) {
			if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("Server shutdown error", "addr", srv.Addr, "error", err)
			}
		}
	}
	shutdownTelemetry := func() {
		if opts.TelemetryShutdown == nil {
			return
		}
		telemetryCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := opts.TelemetryShutdown(telemetryCtx); err != nil {
			slog.Warn("Telemetry shutdown error", "error", err)
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("Received shutdown signal", "signal", sig)
	case <-ctx.Done():
		slog.Info("Context cancelled; shutting down")
	case err := <-serverErr:
		slog.Error("Server failed to start", "error", err)
		shutdown(5 * time.Second)
		shutdownTelemetry()
		return err
	}

	// Phase 1: drain load balancer traffic.
	if opts.SetDraining != nil {
		opts.SetDraining()
	}
	if opts.DrainWait > 0 {
		slog.Info("Waiting for traffic to drain", "duration", opts.DrainWait)
		time.Sleep(opts.DrainWait)
	}

	// Phase 2: graceful shutdown of HTTP servers.
	slog.Info("Starting graceful shutdown")
	shutdown(25 * time.Second)

	// Phase 3: cleanup (e.g. drain callback dispatcher).
	if opts.Cleanup != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		opts.Cleanup(cleanupCtx)
		cancel()
	}

	shutdownTelemetry()

	slog.Info("Shutdown complete")
	return nil
}
