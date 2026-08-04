package docker

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"testing"
)

// Isolation tiers select a Kubernetes RuntimeClass; the Docker backend rejects
// everything but the runc default before touching the daemon.
func TestApply_RejectsRuntimeClassTiers(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{} // Apply must reject before using the client

	for _, tier := range []string{deployment.RuntimeClassGvisor, deployment.RuntimeClassKata} {
		_, err := o.Apply(context.Background(), &deployment.Request{ID: "app", Image: "nginx", RuntimeClass: tier})
		if !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("tier %q: want validation error, got %v", tier, err)
		}
	}
}
