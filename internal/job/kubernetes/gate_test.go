package kubernetes

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func gateProcess(t *testing.T, command string) (*exec.Cmd, string, string) {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "space ' $(literal)")
	if err := os.MkdirAll(filepath.Join(workspace, ".sidecar"), 0o755); err != nil {
		t.Fatal(err)
	}
	args := gatedCommand(command, workspace, 5)
	marker := filepath.Join(workspace, "started")
	args[len(args)-1] = marker // production uses kubelet's termination-message file
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
	cmd.Dir = workspace
	return cmd, args[4], marker
}

func TestGate_WaitsAndPreservesShellCommand(t *testing.T) {
	cmd, ready, marker := gateProcess(t, `printf '%s\n' 'a b' | tr ' ' _ > output; printf '%s' "$GATE_TEST" >> output; exit 7`)
	cmd.Env = append(os.Environ(), "GATE_TEST=literal $value")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("gate exited before readiness: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("started before readiness: %v", err)
	}
	if err := os.WriteFile(ready, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := <-done
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("want exit 7: %v", err)
	}
	output, err := os.ReadFile(filepath.Join(cmd.Dir, "output"))
	if err != nil || string(output) != "a_b\nliteral $value" {
		t.Fatalf("shell semantics changed: %q, %v", output, err)
	}
	started, err := os.ReadFile(marker)
	if err != nil || !strings.HasPrefix(string(started), executionPrefix) {
		t.Fatalf("missing execution record: %q, %v", started, err)
	}
}

func TestGate_Timeout(t *testing.T) {
	cmd, _, marker := gateProcess(t, "echo must-not-run")
	cmd.Args[5] = "2" // two 50ms polls
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 125 {
		t.Fatalf("expected gate timeout: %q %v", out, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("timeout must not record execution")
	}
}

func TestGate_TerminationWhileWaiting(t *testing.T) {
	cmd, _, marker := gateProcess(t, "echo must-not-run")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("terminated gate unexpectedly succeeded")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("cancelled gate must not record execution")
	}
}
