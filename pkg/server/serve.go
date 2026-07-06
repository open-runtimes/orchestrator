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

// Options configures Serve. Handler and ports are required; the hooks are
// optional.
type Options struct {
	Handler        http.Handler
	MetricsHandler http.Handler // served at GET /metrics on MetricsPort
	Port           string
	MetricsPort    string

	// DrainWait is how long to keep serving after readiness flips to
	// shutting-down, so load balancers stop routing here (phase 1 of shutdown).
	DrainWait time.Duration
	// SetDraining flips readiness to shutting-down at the start of shutdown.
	SetDraining func()
	// Cleanup runs after the HTTP servers have shut down (phase 3), e.g. to
	// drain a callback dispatcher.
	Cleanup func(context.Context)
}

// Serve runs the API + metrics HTTP servers until SIGINT/SIGTERM or a server
// error, then performs the three-phase graceful shutdown (drain traffic →
// shut down servers → run cleanup). It returns nil on a clean shutdown.
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

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", opts.MetricsHandler)
	metricsServer := &http.Server{
		Addr:         ":" + opts.MetricsPort,
		Handler:      metricsMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)

	go func() {
		slog.Info("Starting API server", "port", opts.Port)
		if err := apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	go func() {
		slog.Info("Starting metrics server", "port", opts.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	shutdown := func(timeout time.Duration) {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := apiServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("API server shutdown error", "error", err)
		}
		if err := metricsServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Metrics server shutdown error", "error", err)
		}
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("Received shutdown signal", "signal", sig)
	case err := <-serverErr:
		slog.Error("Server failed to start", "error", err)
		shutdown(5 * time.Second)
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
		defer cancel()
		opts.Cleanup(cleanupCtx)
	}

	slog.Info("Shutdown complete")
	return nil
}
