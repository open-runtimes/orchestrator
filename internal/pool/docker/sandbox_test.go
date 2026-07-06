package docker

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"orchestrator/pkg/pool"
	"testing"
)

// Sandbox tiers select a Kubernetes RuntimeClass; the Docker backend rejects
// any non-runc pool at construction (config load) time.
func TestNewOrchestrator_RejectsSandboxTiers(t *testing.T) {
	for _, sandbox := range []string{deployment.SandboxGvisor, deployment.SandboxKata} {
		_, err := NewOrchestrator(context.Background(), Config{Pools: []pool.Pool{
			{ID: "p", Image: "node:20", Sandbox: sandbox},
		}})
		if !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("sandbox %q: want validation error, got %v", sandbox, err)
		}
	}
	if _, err := NewOrchestrator(context.Background(), Config{Pools: []pool.Pool{
		{ID: "p", Image: "node:20", Sandbox: deployment.SandboxRunc},
	}}); err != nil {
		t.Errorf("runc pool: want ok, got %v", err)
	}
}
