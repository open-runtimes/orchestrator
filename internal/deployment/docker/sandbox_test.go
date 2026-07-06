package docker

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"testing"
)

// Sandbox tiers select a Kubernetes RuntimeClass; the Docker backend rejects
// everything but the runc default before touching the daemon.
func TestApply_RejectsSandboxTiers(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{} // Apply must reject before using the client

	for _, sandbox := range []string{deployment.SandboxGvisor, deployment.SandboxKata} {
		err := o.Apply(context.Background(), &deployment.Request{ID: "app", Image: "nginx", Sandbox: sandbox})
		if !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("sandbox %q: want validation error, got %v", sandbox, err)
		}
	}
}
