package sandbox

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/observability"
	"orchestrator/internal/pool"
	"orchestrator/internal/workload"
	"regexp"
	"strings"
	"time"
)

// Validation limits, shared scale with the deployments service.
const (
	maxIDLength    = 63
	maxTimeoutSecs = 3600
	maxArtifacts   = 64
	defaultTimeout = 300
	maxPorts       = 16
	// inlinePrefix names generated ids for poolless sandboxes, where there is no
	// pool name to take one from.
	inlinePrefix = "sbx"

	// tokenBytes sizes the capability token in the hostname. 128 bits, because
	// reaching the URL is sufficient to execute code inside the sandbox — the
	// 32-bit ids that address activations would be guessable.
	tokenBytes = 16
)

var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Service manages sandboxes. Stateless — pools come from config, and all
// sandbox state lives in the backend.
type Service struct {
	orchestrator Orchestrator
	metrics      *observability.Metrics // may be nil in tests
	pools        map[string]*pool.Pool
	artifacts    *artifact.Registry
}

// NewService creates a sandbox service over the configured sandbox pools.
func NewService(orchestrator Orchestrator, metrics *observability.Metrics, pools []pool.Pool, artifacts *artifact.Registry) *Service {
	return &Service{
		orchestrator: orchestrator,
		metrics:      metrics,
		pools:        pool.ByID(pools),
		artifacts:    artifacts,
	}
}

// Pools lists the configured sandbox pools with live counts.
func (s *Service) Pools(ctx context.Context) ([]pool.Status, error) {
	return s.orchestrator.Pools(ctx)
}

// Pool returns one sandbox pool's status.
func (s *Service) Pool(ctx context.Context, poolID string) (*pool.Status, error) {
	return pool.StatusFor(ctx, s.orchestrator, s.pools, poolID)
}

// Create validates the request (applying defaults and the pool's idle
// ceiling), mints the capability token its hostname carries, and claims a warm
// pod — blocking until the sandbox's contract is served.
func (s *Service) Create(ctx context.Context, req *Request) (*Status, error) {
	if err := s.validateSource(req); err != nil {
		return nil, err
	}
	// The id comes first: a poolless sandbox's pool is keyed by it, which is what
	// keeps the pod created for this request from being offered to another.
	if req.ID == "" {
		id, err := generateID(cmp.Or(req.Pool, inlinePrefix))
		if err != nil {
			return nil, apperrors.Internal("sandbox.generateID", err)
		}
		req.ID = id
	}
	// Either a declared pool, or the shape this request asked for. Everything
	// downstream takes a pool, so a poolless sandbox brings its own — a pool of
	// one, created on demand and never offered to anybody else.
	p, ok := s.pools[req.Pool]
	if req.Pool == "" {
		p, ok = InlinePool(req), true
	}
	if !ok {
		return nil, apperrors.NotFound("pool", req.Pool)
	}
	if err := s.validate(req, p); err != nil {
		return nil, err
	}
	token, err := mintToken()
	if err != nil {
		return nil, apperrors.Internal("sandbox.mintToken", err)
	}
	req.Token = token

	// The sandbox's URL is a secret, so it is never logged — the id is.
	logger := slog.With("poolId", req.Pool, "sandboxId", req.ID)
	start := time.Now()
	if s.metrics != nil {
		s.metrics.RecordPoolActivationStarted(ctx, MetricKind, req.Pool)
	}
	status, err := s.orchestrator.Create(ctx, req)
	if s.metrics != nil {
		success := err == nil && status != nil && status.State != StateFailed
		s.metrics.RecordPoolActivationFinished(ctx, MetricKind, req.Pool, success, time.Since(start).Seconds())
	}
	if err != nil {
		logger.Error("Sandbox creation failed", "error", err)
		return nil, err
	}
	logger.Info("Sandbox created", "status", status.State)
	return status, nil
}

// Status returns one sandbox.
func (s *Service) Status(ctx context.Context, id string) (*Status, error) {
	return s.orchestrator.Status(ctx, id)
}

// List returns the live sandboxes.
func (s *Service) List(ctx context.Context) ([]Status, error) {
	return s.orchestrator.List(ctx)
}

// Delete tears a sandbox down. Its URL dies with it: the token lives only as a
// label on the pod being deleted, so a leaked URL is dead on teardown.
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := s.orchestrator.Delete(ctx, id); err != nil {
		return err
	}
	slog.Info("Sandbox deleted", "sandboxId", id)
	return nil
}

// validateSource enforces the one choice a create has to make: claim from a
// pool, or describe a pod for this sandbox alone. Both would be ambiguous —
// which image wins — and neither leaves nothing to run.
func (s *Service) validateSource(req *Request) error {
	switch {
	case req.Pool != "" && req.Image != "":
		return apperrors.Validation("pool", "give either pool (claim from warm capacity) or image (create a pod for this sandbox), not both")
	case req.Pool == "" && req.Image == "":
		return apperrors.Validation("pool", "pool or image is required")
	case req.Pool != "":
		return nil
	}
	if req.Port <= 0 || req.Port > 65535 {
		return apperrors.Validation("port", "port is required with image, and must be 1-65535")
	}
	if req.CPU < 0 {
		return apperrors.Validation("cpu", "cpu must not be negative")
	}
	if req.Memory < 0 {
		return apperrors.Validation("memory", "memory must not be negative")
	}
	for i := range req.Volumes {
		if err := req.Volumes[i].Validate(fmt.Sprintf("volumes[%d]", i)); err != nil {
			return err
		}
	}
	return nil
}

// InlinePool is the pool of one a poolless sandbox brings with it: sized zero so
// nothing replenishes it, cold so the claim creates the pod it needs, and keyed
// by the sandbox's own id so that pod is never offered to another sandbox.
//
// Mounting is inferred from the artifacts, as it is for a job or a revision: the
// pod is built for this request, so the capability need not be declared ahead of
// it.
func InlinePool(req *Request) *pool.Pool {
	return &pool.Pool{
		ID:           req.ID,
		Image:        req.Image,
		Port:         req.Port,
		CPU:          req.CPU,
		Memory:       req.Memory,
		RuntimeClass: req.RuntimeClass,
		Volumes:      req.Volumes,
		Mounts:       artifact.HasMount(req.Artifacts),
		Size:         0,
		Burst:        pool.BurstCold,
	}
}

func (s *Service) validate(req *Request, p *pool.Pool) error {
	// No command check: a sandbox whose pool names none runs the agent the
	// backend installs into its workspace, which is the point — the image is
	// just a runtime, and it serves the contract without implementing it.
	if req.ID != "" {
		if len(req.ID) > maxIDLength {
			return apperrors.Validation("id", fmt.Sprintf("sandbox ID exceeds maximum length of %d", maxIDLength))
		}
		if !idPattern.MatchString(req.ID) {
			return apperrors.Validation("id", "sandbox ID must be an RFC-1123 label (lowercase alphanumeric, interior hyphens)")
		}
	}
	// An omitted timeout takes the default; an explicit 0 is left alone, because
	// it means "no bound" — overwriting it would cut the long-lived sessions
	// (terminals, language servers) that ask for it.
	switch {
	case req.TimeoutSeconds == nil:
		req.TimeoutSeconds = ptrTo(defaultTimeout)
	case *req.TimeoutSeconds < 0 || *req.TimeoutSeconds > maxTimeoutSecs:
		return apperrors.Validation("timeoutSeconds",
			fmt.Sprintf("timeout must be between 0 (no bound) and %d seconds", maxTimeoutSecs))
	}
	if req.IdleTimeoutSeconds < 0 {
		return apperrors.Validation("idleTimeoutSeconds", "idle timeout must be non-negative")
	}
	// A pool's ceiling is operator policy: an abandoned sandbox holds a warm
	// pod hostage, so "until DELETE" is only honored where the pool allows it.
	if p.MaxIdleSeconds > 0 {
		if req.IdleTimeoutSeconds > p.MaxIdleSeconds {
			return apperrors.Validation("idleTimeoutSeconds",
				fmt.Sprintf("idle timeout exceeds pool %q maximum of %ds", p.ID, p.MaxIdleSeconds))
		}
		if req.IdleTimeoutSeconds == 0 {
			req.IdleTimeoutSeconds = p.MaxIdleSeconds
		}
	}
	if err := validatePorts(req.Ports, p.Port); err != nil {
		return err
	}
	if len(req.Artifacts) > maxArtifacts {
		return apperrors.Validation("artifacts", fmt.Sprintf("artifacts exceed maximum of %d", maxArtifacts))
	}
	// Mounting is a property of the pod, fixed when the warm pod was created:
	// the sidecar performing it runs privileged and the workspace propagates. So
	// the pool decides whether its sandboxes may mount, and this is where a
	// request that cannot be honoured is refused — before a pod is claimed for
	// it.
	if !p.Mounts && artifact.HasMount(req.Artifacts) {
		return apperrors.Validation("artifacts", fmt.Sprintf(
			"pool %q does not allow mounts: set mounts on the pool to give its pods the capability", p.ID))
	}
	for i, a := range req.Artifacts {
		if err := s.artifacts.Validate(i, a); err != nil {
			return err
		}
	}
	return nil
}

func ptrTo[T any](v T) *T { return &v }

// validatePorts checks the extra ports a sandbox asks for. The sidecar's own
// data and admin ports are refused: they are the machinery's, and exposing the
// admin one would hand out the claim surface and the request counters.
func validatePorts(ports []int, primary int) error {
	if len(ports) > maxPorts {
		return apperrors.Validation("ports", fmt.Sprintf("ports exceed maximum of %d", maxPorts))
	}
	seen := make(map[int]bool, len(ports))
	for _, port := range ports {
		switch {
		case port < 1 || port > 65535:
			return apperrors.Validation("ports", fmt.Sprintf("port %d is out of range (1-65535)", port))
		case port == workload.DefaultProxyPort || port == workload.DefaultAdminPort:
			return apperrors.Validation("ports", fmt.Sprintf("port %d is reserved by the sandbox sidecar", port))
		case port == primary:
			return apperrors.Validation("ports", fmt.Sprintf("port %d is the pool's own port and is always served", port))
		case seen[port]:
			return apperrors.Validation("ports", fmt.Sprintf("duplicate port %d", port))
		}
		seen[port] = true
	}
	return nil
}

// generateID mints a collision-resistant RFC-1123 sandbox id. It is an
// identifier, not a credential — see mintToken for the credential.
func generateID(poolID string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := poolID + "-" + hex.EncodeToString(b)
	if len(id) > maxIDLength {
		// Truncation can strand a leading hyphen (invalid RFC-1123 label) if
		// the cut lands inside the pool id at a hyphen boundary.
		id = strings.TrimLeft(id[len(id)-maxIDLength:], "-")
	}
	return id, nil
}

// mintToken mints the sandbox's capability token: the leading DNS label of its
// hostname, and the only thing standing between a stranger and code execution
// inside the sandbox. Independent of the id, which is caller-choosable.
func mintToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
