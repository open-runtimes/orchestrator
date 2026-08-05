// pool-shim is the warm-pod entrypoint: it blocks on a workspace FIFO until
// the deployments-sidecar signals an activation, then execs the payload —
// replacing PID 1, so container exit == workload exit and the pod is
// discarded (never reused) when the workload ends. See docs/pools.md.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"orchestrator/internal/config"
	"orchestrator/internal/proxy"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// logFileName holds the shim's own logs, relative to the workspace. NOT
// stdout/stderr: the container's output stream is the workload's activation
// output — collected verbatim by the pool backends — and shim noise must not
// pollute it.
const logFileName = ".pool-shim.log"

func main() {
	var installTo, agentTo string
	flag.StringVar(&installTo, "install", "", "copy this binary to the given path and exit (the shim-install init container: the pool image is the user's runtime and has no shim)")
	flag.StringVar(&agentTo, "install-agent", "", "also copy the vendored sandbox agent to the given path (sandbox pools: the workload image serves the sandbox contract by running this, whatever the image is)")
	flag.Parse()

	if installTo != "" {
		// The install phase runs in its own init container — its stream is
		// never activation output, so log normally.
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "pool-shim"))
		if err := install(installTo); err != nil {
			slog.Error("Shim install failed", "path", installTo, "error", err)
			os.Exit(1)
		}
		slog.Info("Shim installed", "path", installTo)
		if agentTo != "" {
			if err := installAgent(agentTo); err != nil {
				slog.Error("Sandbox agent install failed", "path", agentTo, "error", err)
				os.Exit(1)
			}
			slog.Info("Sandbox agent installed", "path", agentTo)
		}
		return
	}

	workspace := config.GetEnv("SHARED_VOLUME_PATH", "/workspace")
	slog.SetDefault(slog.New(slog.NewJSONHandler(logSink(workspace), nil)).With("service", "pool-shim"))

	if err := run(workspace); err != nil {
		slog.Error("Shim failed", "error", err)
		os.Exit(1)
	}
}

// logSink opens the workspace log file, falling back to discard — a shim
// that cannot log must still activate, and must never write to the
// workload's output stream.
func logSink(workspace string) io.Writer {
	f, err := os.OpenFile(filepath.Join(workspace, logFileName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return io.Discard
	}
	return f
}

// install copies the running binary to path (0755) so the workload container
// can use it as its entrypoint.
func install(path string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return copyExecutable(self, path)
}

// installAgent copies the vendored open-runtimes/sandbox agent for this
// architecture into the workspace, so a sandbox pool's workload image serves the
// sandbox contract by running it — no matter what that image is, and without the
// image implementing anything.
//
// The binary is static (verified against glibc, musl, and distroless bases), and
// it is vendored into this image at build time by hack/fetch-sandbox-agent.sh
// rather than fetched per pod: a warm pod is created for every sandbox that will
// ever be claimed, and a download here would put GitHub on the creation path.
func installAgent(path string) error {
	dataPath := os.Getenv("KO_DATA_PATH")
	if dataPath == "" {
		return errors.New("KO_DATA_PATH is unset: this image carries no vendored sandbox agent")
	}
	agent := filepath.Join(dataPath, "agent-linux-"+runtime.GOARCH)
	if _, err := os.Stat(agent); err != nil {
		return fmt.Errorf("vendored sandbox agent for linux/%s: %w", runtime.GOARCH, err)
	}
	return copyExecutable(agent, path)
}

// copyExecutable copies src to dst with mode 0755, creating parents.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func run(workspace string) error {
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
