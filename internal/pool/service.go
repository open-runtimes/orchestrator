package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"orchestrator/internal/observability"
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
)

var idPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Service manages pool activations. Stateless — pools come from config, and
// all activation state lives in the backend.
type Service struct {
	orchestrator Orchestrator
	metrics      *observability.Metrics // may be nil in tests
	pools        map[string]*Pool
	artifacts    *artifact.Registry
}

// NewService creates a pool service over the configured pools.
func NewService(orchestrator Orchestrator, metrics *observability.Metrics, pools []Pool, artifacts *artifact.Registry) *Service {
	byID := make(map[string]*Pool, len(pools))
	for i := range pools {
		byID[pools[i].ID] = &pools[i]
	}
	return &Service{orchestrator: orchestrator, metrics: metrics, pools: byID, artifacts: artifacts}
}

// Pools lists the configured pools with live counts.
func (s *Service) Pools(ctx context.Context) ([]Status, error) {
	return s.orchestrator.Pools(ctx)
}

// Pool returns one pool's status.
func (s *Service) Pool(ctx context.Context, poolID string) (*Status, error) {
	return StatusFor(ctx, s.orchestrator, s.pools, poolID)
}

// Activate validates the activation (applying defaults) and late-binds it
// onto a warm pod, blocking until the workload is serving.
func (s *Service) Activate(ctx context.Context, poolID string, act *Activation) (*ActivationStatus, error) {
	if _, ok := s.pools[poolID]; !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	if err := s.validate(act); err != nil {
		return nil, err
	}
	if act.ID == "" {
		act.ID = generateActivationID(poolID)
	}

	logger := slog.With("poolId", poolID, "activationId", act.ID)
	start := time.Now()
	if s.metrics != nil {
		s.metrics.RecordPoolActivationStarted(ctx, MetricKind, poolID)
	}
	status, err := s.orchestrator.Activate(ctx, poolID, act)
	if s.metrics != nil {
		success := err == nil && status != nil && status.State != StateFailed
		s.metrics.RecordPoolActivationFinished(ctx, MetricKind, poolID, success, time.Since(start).Seconds())
	}
	if err != nil {
		logger.Error("Activation failed", "error", err)
		return nil, err
	}
	logger.Info("Activation completed", "status", status.State)
	return status, nil
}

// Status returns one activation.
func (s *Service) Status(ctx context.Context, poolID, activationID string) (*ActivationStatus, error) {
	if _, ok := s.pools[poolID]; !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	return s.orchestrator.Status(ctx, poolID, activationID)
}

// List returns the pool's activations.
func (s *Service) List(ctx context.Context, poolID string) ([]ActivationStatus, error) {
	if _, ok := s.pools[poolID]; !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	return s.orchestrator.List(ctx, poolID)
}

// Deactivate tears an activation down.
func (s *Service) Deactivate(ctx context.Context, poolID, activationID string) error {
	if _, ok := s.pools[poolID]; !ok {
		return apperrors.NotFound("pool", poolID)
	}
	if err := s.orchestrator.Deactivate(ctx, poolID, activationID); err != nil {
		return err
	}
	slog.Info("Activation deactivated", "poolId", poolID, "activationId", activationID)
	return nil
}

func (s *Service) validate(act *Activation) error {
	if act.Command == "" {
		return apperrors.Validation("command", "command is required")
	}
	if act.ID != "" {
		if len(act.ID) > maxIDLength {
			return apperrors.Validation("id", fmt.Sprintf("activation ID exceeds maximum length of %d", maxIDLength))
		}
		if !idPattern.MatchString(act.ID) {
			return apperrors.Validation("id", "activation ID must be an RFC-1123 label (lowercase alphanumeric, interior hyphens)")
		}
	}
	if act.TimeoutSeconds < 0 || act.TimeoutSeconds > maxTimeoutSecs {
		return apperrors.Validation("timeoutSeconds", fmt.Sprintf("timeout must be between 0 and %d seconds", maxTimeoutSecs))
	}
	if act.TimeoutSeconds == 0 {
		act.TimeoutSeconds = defaultTimeout
	}
	if act.IdleTimeoutSeconds < 0 {
		return apperrors.Validation("idleTimeoutSeconds", "idle timeout must be non-negative")
	}
	if len(act.Artifacts) > maxArtifacts {
		return apperrors.Validation("artifacts", fmt.Sprintf("artifacts exceed maximum of %d", maxArtifacts))
	}
	for i, a := range act.Artifacts {
		if err := s.artifacts.Validate(i, a); err != nil {
			return err
		}
	}
	return nil
}

// generateActivationID mints a collision-resistant RFC-1123 activation id.
func generateActivationID(poolID string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	id := poolID + "-" + hex.EncodeToString(b)
	if len(id) > maxIDLength {
		// Truncation can strand a leading hyphen (invalid RFC-1123 label) if
		// the cut lands inside the pool id at a hyphen boundary.
		id = strings.TrimLeft(id[len(id)-maxIDLength:], "-")
	}
	return id
}
