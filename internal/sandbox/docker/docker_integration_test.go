//go:build integration

// Package docker — real-daemon integration tests for the sandbox backend.
//
// These drive actual containers: a sandbox image serving the contract, the
// workload-sidecar fronting it, and the job-sidecar materializing artifacts.
// Requests reach the sandbox from inside the container network (see the note
// above proxyAddr), so what is exercised is the whole Docker path minus Host
// routing, which the edge's own tests cover.
package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/proxy"
	"orchestrator/internal/testutil"
	"orchestrator/pkg/pool"
	"orchestrator/pkg/sandbox"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// sandboxTestImage is deliberately a PLAIN runtime image: it implements no
// sandbox contract and contains no agent. Serving the contract is the installed
// agent's job, and this is the test that proves it.
func sandboxTestImage() string {
	if img := os.Getenv("SANDBOX_IMAGE"); img != "" {
		return img
	}
	return "node:22-slim"
}

func sidecarTestImage() string {
	if img := os.Getenv("WORKLOAD_SIDECAR_IMAGE"); img != "" {
		return img
	}
	return "ko.local/workload-sidecar:latest"
}

func jobSidecarTestImage() string {
	if img := os.Getenv("JOB_SIDECAR_IMAGE"); img != "" {
		return img
	}
	return "ko.local/job-sidecar:latest"
}

// clientImage runs the tests' HTTP calls. It is a throwaway container on the
// sandbox's network rather than an exec inside the worker, because the pool
// image is arbitrary — node:22-slim has no wget, distroless has no shell — and
// the point of the agent is that the image needs to contain nothing.
func clientImage() string {
	if img := os.Getenv("CLIENT_IMAGE"); img != "" {
		return img
	}
	return "alpine:latest"
}

// agentTestImage publishes the contract-serving binary the tests install.
func agentTestImage() string {
	if img := os.Getenv("SANDBOX_AGENT_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/open-runtimes/sandbox:0.1.0"
}

// testPool mirrors what an operator declares for an ordinary runtime image: an
// image and a port, no command — the agent supplies the contract.
func testPool() pool.Pool {
	return pool.Pool{ID: "py", Image: sandboxTestImage(), Port: 3000, Size: 1}
}

func newTestOrchestrator(t *testing.T, networkName string) *Orchestrator {
	t.Helper()
	o, err := NewOrchestrator(t.Context(), Config{
		SidecarImage:    sidecarTestImage(),
		JobSidecarImage: jobSidecarTestImage(),
		AgentImage:      agentTestImage(),
		Pools:           []pool.Pool{testPool()},
		Network:         networkName,
		SandboxDomain:   "sandboxes.test",
		DataPort:        "8081",
	})
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

// testNetwork creates a throwaway Docker network for one test and returns its
// name. Sandboxes get a fresh subnet per test, which is worth doing for its own
// sake (the operator knob is DOCKER_NETWORK) and necessary here: on the shared
// default bridge, addresses are recycled between tests, and a Docker host can
// hold stale L2/L3 state for a recycled address long enough to make a brand-new
// container unreachable — reproducible with plain `docker run` plus curl, with
// no orchestrator in the picture.
func testNetwork(t *testing.T) string {
	t.Helper()
	name := "sbx-it-" + strings.ToLower(t.Name())
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	_ = cli.NetworkRemove(context.WithoutCancel(t.Context()), name) // leftovers from a killed run
	if _, err := cli.NetworkCreate(t.Context(), name, network.CreateOptions{Driver: "bridge"}); err != nil {
		t.Fatalf("create network %s: %v", name, err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(t.Context())
		if err := cli.NetworkRemove(cleanupCtx, name); err != nil {
			t.Logf("network %s not removed: %v", name, err)
		}
	})
	return name
}

// testToken stands in for the 32 hex characters pkg/sandbox mints — stable per
// id so a test can address its sandbox, and textually unrelated to the id, which
// is the property the capability rests on.
func testToken(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:32]
}

// create makes a sandbox and guarantees teardown, mirroring what the service
// does: the token is minted by pkg/sandbox, never by a caller.
func create(t *testing.T, o *Orchestrator, req *sandbox.Request) *sandbox.Status {
	t.Helper()
	if req.Token == "" {
		req.Token = testToken(req.ID)
	}
	t.Cleanup(func() { o.cleanup(context.WithoutCancel(t.Context()), req.ID) })

	status, err := o.Create(t.Context(), req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return status
}

// The tests never dial container IPs from the host. That is not portable —
// Docker Desktop cannot route to them at all, and OrbStack keeps a stale route
// for a recycled IP, so a fresh container inheriting a removed one's address is
// unreachable for a while (reproducible with plain `docker run` + curl, no
// orchestrator involved). The in-process edge runs beside the daemon where this
// is not an issue; here, requests go through nc inside the sandbox's own worker
// container, which is exactly where the network is.

// proxyAddr is the proxy container's address on the container network — what the
// edge resolves and proxies to.
//
// It inspects the container rather than calling Target, which additionally
// probes reachability FROM THE CALLER: the edge runs beside the daemon, so that
// probe is right for the product and wrong for a test that reaches the sandbox
// from inside the network. (Target's negative case — a deleted sandbox resolving
// to nothing — needs no routing and is asserted below.)
func proxyAddr(t *testing.T, o *Orchestrator, id string) string {
	t.Helper()
	var addr string
	if !testutil.WaitFor(t, func() bool {
		info, err := o.inspect(t.Context(), proxyName(id))
		if err != nil || info == nil || info.State == nil || !info.State.Running {
			return false
		}
		ip := containerIP(info.NetworkSettings, o.cfg.Network)
		if ip == "" {
			return false
		}
		addr = net.JoinHostPort(ip, strconv.Itoa(proxy.DefaultProxyPort))
		return true
	}, testutil.WithTimeout(30*time.Second), testutil.WithInterval(500*time.Millisecond)) {
		t.Fatalf("proxy container for %s never came up", id)
	}
	return addr
}

// do performs an HTTP request against the sandbox's proxy from inside the
// container network, optionally naming one of its extra ports the way the edge
// does. It returns the response status and body.
//
// GET and POST go through busybox wget, which prints the status line under -S
// and exits as soon as the response arrives. PUT has no wget equivalent, so it
// goes through nc — which must be held open past the response, because nc closes
// its socket the moment stdin hits EOF and Go's HTTP server reads that as
// "client gone" and cancels the request (a 502 from the harness itself).
func do(t *testing.T, o *Orchestrator, id, addr, method, path, body, port string) (int, string) {
	t.Helper()

	header := ""
	if port != "" {
		header = " --header '" + proxy.HeaderPort + ": " + port + "'"
	}
	url := "http://" + addr + path

	var cmd string
	switch method {
	case http.MethodGet:
		cmd = "wget -T 60 -S -q -O - " + header + " " + url + " 2>&1"
	case http.MethodPost:
		cmd = "wget -T 60 -S -q -O - " + header +
			" --header 'Content-Type: application/json' --post-data " + shellQuote(body) + " " + url + " 2>&1"
	default:
		raw := method + " " + path + " HTTP/1.1\r\nHost: sandbox\r\nConnection: close\r\n"
		if port != "" {
			raw += proxy.HeaderPort + ": " + port + "\r\n"
		}
		raw += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
		cmd = "{ echo " + base64.StdEncoding.EncodeToString([]byte(raw)) +
			" | base64 -d; sleep 3; } | nc " + strings.ReplaceAll(addr, ":", " ")
	}

	out := runClient(t, o, cmd)
	status, rest := splitStatus(t, out)
	return status, rest
}

// splitStatus pulls the status code off a raw response or a wget -S transcript,
// returning it with the body.
func splitStatus(t *testing.T, out string) (int, string) {
	t.Helper()
	_, line, ok := strings.Cut(out, "HTTP/1.1 ")
	if !ok {
		t.Fatalf("no status line in response: %q", out)
	}
	code, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(line, " ", 2)[0]))
	if err != nil {
		t.Fatalf("unparseable status in %q", out)
	}
	// The body is what follows the headers: a blank line for raw responses, and
	// for wget the transcript's own trailing output.
	body := ""
	if _, raw, ok := strings.Cut(out, "\r\n\r\n"); ok {
		body = raw
	} else if lines := strings.Split(out, "\n"); len(lines) > 0 {
		// wget -S indents headers with two spaces; the body follows them.
		for i, l := range lines {
			if !strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "wget:") && strings.TrimSpace(l) != "" {
				body = strings.Join(lines[i:], "\n")
				break
			}
		}
	}
	return code, strings.TrimRight(body, "\n")
}

// shellQuote wraps s for safe use inside the single-quoted shell command built
// above.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// runClient runs a shell command in a throwaway container on the sandbox
// network and returns its combined output.
func runClient(t *testing.T, o *Orchestrator, command string) string {
	t.Helper()
	ctx := t.Context()

	if err := o.pullImageIfNeeded(ctx, clientImage()); err != nil {
		t.Fatalf("pull client image: %v", err)
	}
	created, err := o.client.ContainerCreate(ctx,
		&container.Config{
			Image:      clientImage(),
			Entrypoint: []string{"/bin/sh", "-c"},
			Cmd:        []string{command},
		},
		&container.HostConfig{}, o.networkingConfig(), nil, "")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer o.removeContainer(context.WithoutCancel(ctx), created.ID)

	if err := o.client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start client: %v", err)
	}
	if _, err := o.waitForExit(ctx, created.ID); err != nil {
		t.Fatalf("wait client: %v", err)
	}
	logs, err := o.client.ContainerLogs(ctx, created.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("client logs: %v", err)
	}
	defer logs.Close()

	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, logs); err != nil {
		t.Fatalf("read client logs: %v", err)
	}
	return out.String()
}

// execute runs a command through the sandbox contract and returns its stdout.
func execute(t *testing.T, o *Orchestrator, id, addr, command string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"command": command, "timeoutSeconds": 30})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	status, body := do(t, o, id, addr, http.MethodPost, "/execute", string(payload), "")
	if status != http.StatusOK {
		t.Fatalf("/execute = %d: %s", status, body)
	}
	var result struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode /execute: %v (%s)", err, body)
	}
	if result.ExitCode != 0 {
		t.Fatalf("command %q exited %d: %s", command, result.ExitCode, result.Stderr)
	}
	return result.Stdout
}

// TestSandboxLifecycle covers the whole Docker path: create → serve the
// contract → status/list reconstruction → delete, plus the capability token.
func TestSandboxLifecycle(t *testing.T) {
	o := newTestOrchestrator(t, testNetwork(t))
	status := create(t, o, &sandbox.Request{ID: "it-life", Pool: "py"})

	if status.State != sandbox.StateReady {
		t.Fatalf("state = %s (%s), want ready", status.State, status.Error)
	}
	// The URL is the token's, never the id's.
	if want := "http://s-" + testToken("it-life") + ".sandboxes.test:8081"; status.URL != want {
		t.Errorf("url: want %s, got %s", want, status.URL)
	}
	if strings.Contains(status.URL, "it-life") {
		t.Error("the sandbox id must not appear in its address — it is guessable")
	}
	if status.URLs["3000"] != status.URL {
		t.Errorf("urls: got %v", status.URLs)
	}

	addr := proxyAddr(t, o, "it-life")
	if code, body := do(t, o, "it-life", addr, http.MethodGet, "/healthz", "", ""); code != http.StatusOK || !strings.Contains(body, "ok") {
		t.Errorf("/healthz = %d %q", code, body)
	}
	if out := execute(t, o, "it-life", addr, "echo hello && pwd"); !strings.Contains(out, "hello") || !strings.Contains(out, "/workspace") {
		t.Errorf("/execute stdout = %q", out)
	}

	// Files round-trip, and an exec sees what was written over HTTP.
	if code, body := do(t, o, "it-life", addr, http.MethodPut, "/files/main.txt", "written by the test", ""); code >= 300 {
		t.Fatalf("PUT /files = %d %q", code, body)
	}
	if code, body := do(t, o, "it-life", addr, http.MethodGet, "/files/main.txt", "", ""); code != http.StatusOK || body != "written by the test" {
		t.Errorf("GET /files = %d %q", code, body)
	}
	if out := execute(t, o, "it-life", addr, "cat main.txt"); out != "written by the test" {
		t.Errorf("exec sees the file: got %q", out)
	}

	// Nothing is held in memory: a second orchestrator over the same daemon
	// reconstructs the sandbox from the volume's labels.
	// Reconstruction reads labels, not the network.
	fresh := newTestOrchestrator(t, "")
	reread, err := fresh.Status(t.Context(), "it-life")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if reread.State != sandbox.StateReady || reread.URL != status.URL {
		t.Errorf("reconstructed status = %+v", reread)
	}
	list, err := fresh.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !slicesContainsID(list, "it-life") {
		t.Errorf("List missing the sandbox: %+v", list)
	}

	// A re-used id is a conflict, not a second sandbox.
	if _, err := o.Create(t.Context(), &sandbox.Request{ID: "it-life", Pool: "py", Token: testToken("other")}); !errors.Is(err, apperrors.ErrConflict) {
		t.Errorf("duplicate id: want ErrConflict, got %v", err)
	}

	if err := o.Delete(t.Context(), "it-life"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := o.Status(t.Context(), "it-life"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("after delete: want ErrNotFound, got %v", err)
	}
	// The token dies with the sandbox, so a leaked URL resolves to nothing.
	if target, err := o.Target(t.Context(), testToken("it-life")); err != nil || target != nil {
		t.Errorf("token still resolves after delete: %v (%v)", target, err)
	}
}

// TestSandboxExtraPorts starts a listener on a declared port after creation and
// reaches it through the port hint — the same mechanism the edge drives from the
// hostname.
func TestSandboxExtraPorts(t *testing.T) {
	o := newTestOrchestrator(t, testNetwork(t))
	status := create(t, o, &sandbox.Request{ID: "it-ports", Pool: "py", Ports: []int{5173}})

	if want := "http://s-" + testToken("it-ports") + "-5173.sandboxes.test:8081"; status.URLs["5173"] != want {
		t.Errorf("extra port url: got %v", status.URLs)
	}
	addr := proxyAddr(t, o, "it-ports")

	// Nothing is listening yet: declared but dead is a 502, undeclared is a 404.
	if code, _ := do(t, o, "it-ports", addr, http.MethodGet, "/", "", "5173"); code != http.StatusBadGateway {
		t.Errorf("declared-but-dead port = %d, want 502", code)
	}
	if code, _ := do(t, o, "it-ports", addr, http.MethodGet, "/", "", "9229"); code != http.StatusNotFound {
		t.Errorf("undeclared port = %d, want 404", code)
	}

	// Start one from an exec, as a caller would with a dev server: nothing was
	// listening when the sandbox was created, and no restart is involved.
	// A dev server on the extra port, started from an exec after the sandbox
	// exists — the actual use case. Node is the pool image's own runtime; the
	// sandbox needed no shell tooling of its own.
	execute(t, o, "it-ports", addr,
		`node -e 'require("http").createServer((_,res)=>res.end("dev server")).listen(5173)' >/dev/null 2>&1 &
sleep 1`)

	// Bound inside the container first — separating "the listener never came up"
	// from "the port hint did not route", which are different bugs. /proc/net/tcp
	// works in any Linux image (5173 == 0x1435), unlike netstat or ss.
	if !testutil.WaitFor(t, func() bool {
		return strings.Contains(execute(t, o, "it-ports", addr, "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null"), ":1435")
	}, testutil.WithTimeout(20*time.Second), testutil.WithInterval(time.Second)) {
		t.Fatal("listener never bound in the sandbox")
	}

	if code, body := do(t, o, "it-ports", addr, http.MethodGet, "/", "", "5173"); code != http.StatusOK || !strings.Contains(body, "dev server") {
		t.Fatalf("declared port not reachable through the hint: %d %q", code, body)
	}
}

// TestSandboxArtifacts materializes an artifact into the workspace before the
// sandbox reports ready, and fails the sandbox (rather than the API) when one
// cannot be materialized.
func TestSandboxArtifacts(t *testing.T) {
	o := newTestOrchestrator(t, testNetwork(t))
	status := create(t, o, &sandbox.Request{
		ID:   "it-artifacts",
		Pool: "py",
		Artifacts: artifact.Set{
			&artifact.Write{ID: "cfg", In: "hello from an artifact", Out: "config.txt"},
		},
	})
	if status.State != sandbox.StateReady {
		t.Fatalf("state = %s (%s)", status.State, status.Error)
	}
	if out := execute(t, o, "it-artifacts", proxyAddr(t, o, "it-artifacts"), "cat config.txt"); out != "hello from an artifact" {
		t.Errorf("artifact not materialized: got %q", out)
	}

	// A failed artifact leaves no sandbox behind: no URL was handed out and
	// nothing is running, the Docker analogue of a poisoned pod.
	failed, err := o.Create(t.Context(), &sandbox.Request{
		ID:        "it-artifacts-bad",
		Pool:      "py",
		Token:     testToken("it-artifacts-bad"),
		Artifacts: artifact.Set{&artifact.Read{ID: "missing", In: "does-not-exist.json"}},
	})
	if err != nil {
		t.Fatalf("an artifact failure is a failed sandbox, not an error: %v", err)
	}
	if failed.State != sandbox.StateFailed {
		t.Errorf("state = %s, want failed", failed.State)
	}
	if failed.URL != "" {
		t.Errorf("a failed sandbox must not hand out a URL: %s", failed.URL)
	}
	if _, err := o.Status(t.Context(), "it-artifacts-bad"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("failed sandbox must be cleaned up, got %v", err)
	}
}

// TestSandboxIdleSweep tears down a sandbox whose requests stop, and leaves a
// busy one alone.
func TestSandboxIdleSweep(t *testing.T) {
	o := newTestOrchestrator(t, testNetwork(t))
	create(t, o, &sandbox.Request{ID: "it-idle", Pool: "py", IdleTimeoutSeconds: 60})
	addr := proxyAddr(t, o, "it-idle")
	if code, _ := do(t, o, "it-idle", addr, http.MethodGet, "/healthz", "", ""); code != http.StatusOK {
		t.Fatal("sandbox not serving")
	}

	marks := map[string]idleMark{}
	t0 := time.Now()
	o.now = func() time.Time { return t0 }
	o.reapIdle(t.Context(), marks) // baseline

	// Traffic across the window keeps it alive.
	if code, _ := do(t, o, "it-idle", addr, http.MethodGet, "/healthz", "", ""); code != http.StatusOK {
		t.Fatal("sandbox stopped serving")
	}
	o.now = func() time.Time { return t0.Add(61 * time.Second) }
	o.reapIdle(t.Context(), marks)
	if _, err := o.Status(t.Context(), "it-idle"); err != nil {
		t.Fatalf("a sandbox with fresh traffic must survive: %v", err)
	}

	// No movement across the next window: reaped.
	o.now = func() time.Time { return t0.Add(122 * time.Second) }
	o.reapIdle(t.Context(), marks)
	o.now = func() time.Time { return t0.Add(183 * time.Second) }
	o.reapIdle(t.Context(), marks)
	if _, err := o.Status(t.Context(), "it-idle"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Errorf("idle sandbox must be torn down, got %v", err)
	}
}

// TestSandboxPools reports configured pools with no warm capacity — Docker
// pre-warms nothing, and saying otherwise would be a lie.
func TestSandboxPools(t *testing.T) {
	o := newTestOrchestrator(t, testNetwork(t))
	create(t, o, &sandbox.Request{ID: "it-pools", Pool: "py"})

	pools, err := o.Pools(t.Context())
	if err != nil {
		t.Fatalf("Pools: %v", err)
	}
	if len(pools) != 1 || pools[0].ID != "py" {
		t.Fatalf("Pools: got %+v", pools)
	}
	if pools[0].Warm != 0 {
		t.Errorf("Docker has no warm capacity, got warm=%d", pools[0].Warm)
	}
	if pools[0].Claimed != 1 {
		t.Errorf("claimed: want 1, got %d", pools[0].Claimed)
	}
}

func slicesContainsID(list []sandbox.Status, id string) bool {
	for i := range list {
		if list[i].ID == id {
			return true
		}
	}
	return false
}
