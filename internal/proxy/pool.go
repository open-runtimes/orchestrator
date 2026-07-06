package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sidecar"
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
	token     string // bearer token required on ClaimPath
	workspace string // shared volume: artifacts materialize here, the shim FIFO lives here

	claimed atomic.Bool                // claim gate: the first CAS wins, forever
	state   atomic.Pointer[ClaimState] // published record; written only by the claim winner
}

func newPool(cfg Config) *pool {
	pl := &pool{token: cfg.ClaimToken, workspace: cfg.Workspace}
	pl.state.Store(&ClaimState{})
	return pl
}

func (pl *pool) snapshot() ClaimState { return *pl.state.Load() }

func (pl *pool) publish(s ClaimState) { pl.state.Store(&s) }

// authorized checks Authorization: Bearer <token> in constant time.
func (pl *pool) authorized(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(token), []byte(pl.token)) == 1
}

// handleActivate accepts the activation (POST ClaimPath): 401 bad token, 409
// already claimed or poisoned, 400 undecodable claim, 422 artifacts or shim
// signaling failed (poisoned), 200 claimed and signaled.
func (p *Proxy) handleActivate(w http.ResponseWriter, r *http.Request) {
	pl := p.pool
	if !pl.authorized(r) {
		http.Error(w, "invalid claim token", http.StatusUnauthorized)
		return
	}
	// Exactly-one: the first request flips the flag and wins; every later
	// request — even while the winner is still materializing artifacts, and
	// forever once claimed or poisoned — gets 409 and retries another pod.
	if !pl.claimed.CompareAndSwap(false, true) {
		writeClaimState(w, http.StatusConflict, pl.snapshot())
		return
	}
	var req ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// The claim is spent: a pod is never resold, so an undecodable claim
		// poisons it and replenishment replaces it.
		pl.publish(ClaimState{Claimed: true, Failed: true, Error: "decode claim: " + err.Error()})
		writeClaimState(w, http.StatusBadRequest, pl.snapshot())
		return
	}
	pl.publish(ClaimState{Claimed: true, ActivationID: req.ActivationID})
	if err := p.activate(r.Context(), req); err != nil {
		pl.publish(ClaimState{Claimed: true, ActivationID: req.ActivationID, Failed: true, Error: err.Error()})
		writeClaimState(w, http.StatusUnprocessableEntity, pl.snapshot())
		return
	}
	writeClaimState(w, http.StatusOK, pl.snapshot())
}

// handleClaimState reports the authoritative claim record (GET
// ClaimStatePath) — the backends' reconcile / orphan-GC source of truth.
// Unauthenticated by design: it leaks only an activation id.
func (p *Proxy) handleClaimState(w http.ResponseWriter, _ *http.Request) {
	writeClaimState(w, http.StatusOK, p.pool.snapshot())
}

func writeClaimState(w http.ResponseWriter, status int, s ClaimState) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s)
}

// activate materializes the payload onto the pod: artifacts into the
// workspace, one ShimExec line down the FIFO, and — for HTTP claims — the
// late-bound data plane. Any error poisons the pod (the caller publishes it).
func (p *Proxy) activate(ctx context.Context, req ClaimRequest) error {
	timeoutSeconds := req.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = int(p.cfg.Timeout / time.Second)
	}

	// Same materialization path as the job sidecar's pre phase: pre-job
	// artifacts in dependency order against the shared workspace. No report
	// sink — the claim response is the result.
	runner := sidecar.NewRunner(req.ActivationID, p.pool.workspace, timeoutSeconds, artifact.DefaultRegistry())
	if err := runner.RunPre(ctx, req.Artifacts); err != nil {
		return err
	}

	if err := p.pool.signalShim(ctx, ShimExec{
		Command:     req.Command,
		Environment: req.Environment,
		WorkDir:     p.pool.workspace,
	}); err != nil {
		return err
	}

	if req.Port > 0 {
		p.arm(req)
	}
	return nil
}

// arm late-binds the data plane onto the claimed workload: reverse target
// TargetHost:Port, the claim's per-request timeout, and the readiness prober
// — from here /ready means the workload serves.
func (p *Proxy) arm(req ClaimRequest) {
	cfg := p.cfg
	cfg.Target = net.JoinHostPort(cfg.TargetHost, strconv.Itoa(req.Port))
	if req.TimeoutSeconds > 0 {
		cfg.Timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	b := newBinding(cfg)
	p.bind.Store(b)
	go b.prober.run(p.runCtx)
}

// signalShim writes the single ShimExec JSON line that turns the idle shim
// into the workload.
func (pl *pool) signalShim(ctx context.Context, payload ShimExec) error {
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
	path := filepath.Join(pl.workspace, ShimFIFOName)
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
