package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sidecar"
	"orchestrator/internal/workload"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// shimOpenTimeout bounds how long a claim waits for the shim's read end of
// the FIFO to appear. If it never does, the shim isn't running and the pod
// must be poisoned rather than left half-activated.
const shimOpenTimeout = 10 * time.Second

// pool is the claim surface armed by Config.ClaimToken: the proxy starts with
// no target and accepts exactly one activation. The pod is the serialization
// point — racing pool backends get 409 and retry another warm pod, so the
// service stays stateless. See claim.go for the wire protocol.
type pool struct {
	token     string // bearer token required on workload.ClaimPath
	workspace string // shared volume: artifacts materialize here, the shim FIFO lives here

	claimed atomic.Bool                         // claim gate: the first CAS wins, forever
	state   atomic.Pointer[workload.ClaimState] // published record; written only by the claim winner
}

func newPool(cfg Config) *pool {
	pl := &pool{token: cfg.ClaimToken, workspace: cfg.Workspace}
	pl.state.Store(&workload.ClaimState{})
	return pl
}

func (pl *pool) snapshot() workload.ClaimState { return *pl.state.Load() }

func (pl *pool) publish(s workload.ClaimState) { pl.state.Store(&s) }

// authorized checks Authorization: Bearer <token> in constant time.
func (pl *pool) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(token), []byte(pl.token)) == 1
}

// handleActivate accepts the activation (POST workload.ClaimPath): 401 bad token, 409
// already claimed or poisoned, 400 undecodable claim, 422 artifacts or shim
// signaling failed (poisoned), 200 claimed and signaled.
func (p *Proxy) handleActivate(w http.ResponseWriter, r *http.Request) {
	pl := p.pool
	if !pl.authorized(r) {
		http.Error(w, "invalid claim token", http.StatusUnauthorized)
		return
	}
	// This pod is being torn down. A claim accepted now would hand its caller a
	// workload that is about to vanish, so it is refused the way a racing loser
	// is: 409, and the backend tries another warm pod.
	if p.closing.Load() {
		writeClaimState(w, http.StatusConflict, workload.ClaimState{Failed: true, Error: "pod is shutting down"})
		return
	}
	// Exactly-one: the first request flips the flag and wins; every later
	// request — even while the winner is still materializing artifacts, and
	// forever once claimed or poisoned — gets 409 and retries another pod.
	if !pl.claimed.CompareAndSwap(false, true) {
		writeClaimState(w, http.StatusConflict, pl.snapshot())
		return
	}
	var req workload.ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// The claim is spent: a pod is never resold, so an undecodable claim
		// poisons it and replenishment replaces it.
		pl.publish(workload.ClaimState{Claimed: true, Failed: true, Error: "decode claim: " + err.Error()})
		writeClaimState(w, http.StatusBadRequest, pl.snapshot())
		return
	}
	pl.publish(workload.ClaimState{Claimed: true, ActivationID: req.ActivationID})
	if err := p.activate(r.Context(), req); err != nil {
		pl.publish(workload.ClaimState{Claimed: true, ActivationID: req.ActivationID, Failed: true, Error: err.Error()})
		writeClaimState(w, http.StatusUnprocessableEntity, pl.snapshot())
		return
	}
	writeClaimState(w, http.StatusOK, pl.snapshot())
}

// handleClaimState reports the authoritative claim record (GET
// workload.ClaimStatePath) — the backends' reconcile / orphan-GC source of truth.
// Unauthenticated by design: it leaks only an activation id.
func (p *Proxy) handleClaimState(w http.ResponseWriter, _ *http.Request) {
	writeClaimState(w, http.StatusOK, p.pool.snapshot())
}

func writeClaimState(w http.ResponseWriter, status int, s workload.ClaimState) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s)
}

// activate materializes the payload onto the pod: artifacts into the
// workspace, one workload.ShimExec line down the FIFO, then the late-bound data
// plane. Any error poisons the pod (the caller publishes it).
func (p *Proxy) activate(ctx context.Context, req workload.ClaimRequest) error {
	// Artifacts keep a bound even when the requests they serve do not: an
	// unbounded claim means long-lived SESSIONS, not an unbounded download.
	timeoutSeconds := int(p.cfg.Timeout / time.Second)
	if req.TimeoutSeconds != nil && *req.TimeoutSeconds > 0 {
		timeoutSeconds = *req.TimeoutSeconds
	}

	// A mount needs a privileged sidecar and a propagating workspace, which are
	// pod properties fixed when the warm pod was created. The API rejects a
	// mount against a pool without the capability; refusing it here too means a
	// workload never starts believing it got something it did not.
	if artifact.HasMount(req.Artifacts) && !p.cfg.Mounts {
		return errors.New("this pool cannot mount: set mounts on the pool to give its pods the capability")
	}

	// Same materialization path as the job sidecar's pre phase: pre-job
	// artifacts in dependency order against the shared workspace. No report
	// sink — the claim response is the result.
	opts := []sidecar.Option{sidecar.WithS3Credentials(p.cfg.S3)}
	if p.mounter != nil {
		opts = append(opts, sidecar.WithMounter(p.mounter))
	}
	runner := sidecar.NewRunner(req.ActivationID, p.pool.workspace, timeoutSeconds, artifact.DefaultRegistry(), opts...)
	if err := runner.RunPre(ctx, req.Artifacts); err != nil {
		return err
	}

	// Mounts come after the images they mount are materialized and before the
	// workload is signalled, so the exec'd payload finds them in place. The
	// runner is kept for shutdown: bidirectional propagation means an
	// unreleased mount outlives this pod on its node.
	if err := runner.Mount(ctx, req.Artifacts); err != nil {
		return err
	}
	p.mounts.Store(runner)
	// Teardown may have begun while this claim was materializing, and release
	// runs exactly once. If it already ran, undo what we just did rather than
	// leave a propagated mount on the node.
	if p.closing.Load() {
		runner.Release()
		return errors.New("pod began shutting down while the claim was materializing")
	}

	if err := p.pool.signalShim(ctx, workload.ShimExec{
		Command:     req.Command,
		Environment: req.Environment,
		WorkDir:     p.pool.workspace,
	}); err != nil {
		return err
	}

	p.arm(req)
	return nil
}

// arm late-binds the data plane onto the claimed workload: reverse target
// TargetHost:Port (plus any secondary ports), the claim's per-request timeout,
// and the readiness prober
// — from here /ready means the workload serves.
func (p *Proxy) arm(req workload.ClaimRequest) {
	cfg := p.cfg
	cfg.Target = net.JoinHostPort(cfg.TargetHost, strconv.Itoa(req.Port))
	// A claim that states its timeout wins, including a 0 that asks for no
	// bound at all — the reason this is a pointer.
	if req.TimeoutSeconds != nil {
		cfg.Timeout = time.Duration(*req.TimeoutSeconds) * time.Second
	}
	// Secondary ports are addressable but never probed — see workload.ClaimRequest.Ports.
	cfg.ExtraPorts = req.Ports
	b := newBinding(cfg)
	p.bind.Store(b)
	go b.prober.run(p.runCtx)
}

// signalShim writes the single workload.ShimExec JSON line that turns the idle shim
// into the workload.
func (pl *pool) signalShim(ctx context.Context, payload workload.ShimExec) error {
	fifo, err := pl.openFIFO(ctx)
	if err != nil {
		return fmt.Errorf("open shim FIFO: %w", err)
	}
	defer fifo.Close()
	if err := json.NewEncoder(fifo).Encode(payload); err != nil {
		return fmt.Errorf("write shim exec: %w", err)
	}
	return nil
}

// openFIFO opens the workspace FIFO write-only. A plain blocking open would
// hang forever if the shim is gone, so open O_NONBLOCK — which fails until a
// reader exists — and retry briefly; no reader within shimOpenTimeout means
// the shim isn't there.
func (pl *pool) openFIFO(ctx context.Context) (*os.File, error) {
	path := filepath.Join(pl.workspace, workload.ShimFIFOName)
	deadline := time.Now().Add(shimOpenTimeout)
	for {
		fifo, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil || time.Now().After(deadline) {
			return fifo, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
