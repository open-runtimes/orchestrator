//go:build linux

// mount-agent is the node mount agent: a DaemonSet that serves mounting pods'
// workspaces as CSI inline volumes and performs their artifact mounts from the
// node, so the pods need no privileged container
// (docs/design/node-mount-agent.md).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"orchestrator/internal/config"
	"orchestrator/internal/mountagent"
	"orchestrator/internal/mountagent/node"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
)

// releaseVersion is set by the release build and reported to kubelet as the
// CSI driver's vendor version.
//
//nolint:gochecknoglobals // Go linker injection requires a package variable.
var releaseVersion = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("Mount agent failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	nodeID := config.GetEnv("NODE_NAME", "")
	if nodeID == "" {
		return errors.New("NODE_NAME is required")
	}
	// The chart binds the API to the node's own IP (the agent runs on the host
	// network); the per-pod token, not the address, is the boundary.
	listen := config.GetEnv("LISTEN_ADDR", ":"+strconv.Itoa(mountagent.DefaultPort))
	// Namespaces whose pods may use the driver: the job namespace. Empty
	// allows any, for development clusters.
	var namespaces []string
	if raw := config.GetEnv("ALLOWED_NAMESPACES", ""); raw != "" {
		namespaces = strings.Split(raw, ",")
	}
	pluginDir := "/var/lib/kubelet/plugins/" + mountagent.Driver
	csiEndpoint := pluginDir + "/csi.sock"

	agent, err := node.New(releaseVersion, nodeID, pluginDir, namespaces)
	if err != nil {
		return err
	}

	if err := os.Remove(csiEndpoint); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	csiListener, err := (&net.ListenConfig{}).Listen(ctx, "unix", csiEndpoint)
	if err != nil {
		return err
	}
	grpcServer := grpc.NewServer()
	agent.RegisterCSI(grpcServer)

	// Any pod can reach this port, so bound what an unauthenticated client can
	// hold open. WriteTimeout covers the slowest mount (a large image under
	// contention).
	apiListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return err
	}
	api := &http.Server{
		Handler:           agent.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      3 * time.Minute,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    8 << 10,
	}

	errs := make(chan error, 2)
	go func() { errs <- grpcServer.Serve(csiListener) }()
	go func() { errs <- api.Serve(netutil.LimitListener(apiListener, 256)) }()
	slog.Info("Mount agent started", "node", nodeID, "csi", csiEndpoint, "api", listen, "namespaces", namespaces)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	grpcServer.GracefulStop()
	return api.Shutdown(shutdown)
}
