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
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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
	driver := config.GetEnv("DRIVER_NAME", mountagent.DefaultDriver)
	nodeID := config.GetEnv("NODE_NAME", "")
	if nodeID == "" {
		return errors.New("NODE_NAME is required")
	}
	csiEndpoint := config.GetEnv("CSI_ENDPOINT", "/csi/csi.sock")
	// The chart binds the API to the node's own IP (the agent runs on the host
	// network); the token, not the address, is the boundary.
	listen := config.GetEnv("LISTEN_ADDR", ":"+strconv.Itoa(mountagent.DefaultPort))
	stateDir := config.GetEnv("STATE_DIR", "/var/lib/kubelet/plugins/"+driver)

	agent, err := mountagent.New(driver, releaseVersion, nodeID, stateDir)
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

	api := &http.Server{Addr: listen, Handler: agent.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errs := make(chan error, 2)
	go func() { errs <- grpcServer.Serve(csiListener) }()
	go func() { errs <- api.ListenAndServe() }()
	slog.Info("Mount agent started", "driver", driver, "node", nodeID, "csi", csiEndpoint, "api", listen)

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
