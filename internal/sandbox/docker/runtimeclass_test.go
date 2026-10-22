package docker

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/deployment"
	"orchestrator/internal/sandbox"
	"testing"
)

// Isolation tiers select a Kubernetes RuntimeClass; the Docker backend must
// reject everything but the runc default rather than silently run untrusted
// code on runc.
func TestCreate_RejectsRuntimeClassTiers(t *testing.T) {
	t.Parallel()
	o := &Orchestrator{} // Create must reject before using the client

	for _, tier := range []string{deployment.RuntimeClassGvisor, deployment.RuntimeClassKata} {
		_, err := o.Create(context.Background(), &sandbox.Request{ID: "sbx", Image: "nginx", RuntimeClass: tier})
		if !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("tier %q: want validation error, got %v", tier, err)
		}
	}
}
