// pool-shim is the warm-pod entrypoint: it blocks on a workspace FIFO until
// the deployments-sidecar signals an activation, then execs the payload —
// replacing PID 1, so container exit == workload exit and the pod is
// discarded (never reused) when the workload ends. See docs/design/pools.md.
package main

import (
	"encoding/json"
	"log/slog"
	"orchestrator/internal/config"
	"orchestrator/internal/proxy"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "pool-shim"))

	if err := run(); err != nil {
		slog.Error("Shim failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	workspace := config.GetEnv("SHARED_VOLUME_PATH", "/workspace")
	fifoPath := filepath.Join(workspace, proxy.ShimFIFOName)

	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil && !os.IsExist(err) {
		return err
	}
	slog.Info("Warm and waiting for activation", "fifo", fifoPath)

	// Opening the FIFO read-only blocks until the sidecar opens the write
	// end — the wait itself costs nothing.
	fifo, err := os.Open(fifoPath)
	if err != nil {
		return err
	}
	var payload proxy.ShimExec
	if err := json.NewDecoder(fifo).Decode(&payload); err != nil {
		_ = fifo.Close()
		return err
	}
	_ = fifo.Close()

	shell, err := exec.LookPath("sh")
	if err != nil {
		shell = "/bin/sh"
	}
	env := os.Environ()
	for k, v := range payload.Environment {
		env = append(env, k+"="+v)
	}
	if payload.WorkDir != "" {
		if err := os.Chdir(payload.WorkDir); err != nil {
			return err
		}
	}

	slog.Info("Activating", "command", payload.Command)
	// Exec replaces this process: the workload becomes PID 1 and its exit is
	// the container's exit.
	return syscall.Exec(shell, []string{"sh", "-c", payload.Command}, env)
}
