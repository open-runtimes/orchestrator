package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"regexp"
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
	pools        map[string]*Pool
	artifacts    *artifact.Registry
}

// NewService creates a pool service over the configured pools.
func NewService(orchestrator Orchestrator, pools []Pool, artifacts *artifact.Registry) *Service {
	byID := make(map[string]*Pool, len(pools))
	for i := range pools {
		byID[pools[i].ID] = &pools[i]
	}
	return &Service{orchestrator: orchestrator, pools: byID, artifacts: artifacts}
}

// Pools lists the configured pools with live counts.
func (s *Service) Pools(ctx context.Context) ([]Status, error) {
	return s.orchestrator.Pools(ctx)
}

// Pool returns one pool's status.
func (s *Service) Pool(ctx context.Context, poolID string) (*Status, error) {
	if _, ok := s.pools[poolID]; !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	statuses, err := s.orchestrator.Pools(ctx)
	if err != nil {
		return nil, err
	}
	for i := range statuses {
		if statuses[i].ID == poolID {
			return &statuses[i], nil
		}
	}
	return nil, apperrors.NotFound("pool", poolID)
}

// Activate validates the activation (applying defaults) and late-binds it
// onto a warm pod. Blocks per the orchestrator contract: exec pools until
// exit, HTTP pools until serving.
func (s *Service) Activate(ctx context.Context, poolID string, act *Activation) (*ActivationStatus, error) {
	p, ok := s.pools[poolID]
	if !ok {
		return nil, apperrors.NotFound("pool", poolID)
	}
	if err := s.validate(p, act); err != nil {
		return nil, err
	}
	if act.ID == "" {
		act.ID = generateActivationID(poolID)
	}

	logger := slog.With("poolId", poolID, "activationId", act.ID)
	status, err := s.orchestrator.Activate(ctx, poolID, act)
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

func (s *Service) validate(p *Pool, act *Activation) error {
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
	if !p.HTTP() && act.IdleTimeoutSeconds > 0 {
		return apperrors.Validation("idleTimeoutSeconds", "idle timeout applies only to HTTP pools")
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
		id = id[len(id)-maxIDLength:]
	}
	return id
}
