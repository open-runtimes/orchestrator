// pool-shim is the warm-pod entrypoint: it blocks on a workspace FIFO until
// the deployments-sidecar signals an activation, then execs the payload —
// replacing PID 1, so container exit == workload exit and the pod is
// discarded (never reused) when the workload ends. See docs/design/pools.md.
package main

import (
	"encoding/json"
	"flag"
	"io"
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

	var installTo string
	flag.StringVar(&installTo, "install", "", "copy this binary to the given path and exit (the shim-install init container: the pool image is the user's runtime and has no shim)")
	flag.Parse()

	if installTo != "" {
		if err := install(installTo); err != nil {
			slog.Error("Shim install failed", "path", installTo, "error", err)
			os.Exit(1)
		}
		slog.Info("Shim installed", "path", installTo)
		return
	}

	if err := run(); err != nil {
		slog.Error("Shim failed", "error", err)
		os.Exit(1)
	}
}

// install copies the running binary to path (0755) so the workload container
// can use it as its entrypoint.
func install(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
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
