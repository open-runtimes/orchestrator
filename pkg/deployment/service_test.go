package deployment

import (
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"testing"
)

func TestValidate_Sandbox(t *testing.T) {
	t.Parallel()
	s := &Service{artifacts: artifact.DefaultRegistry(), domain: "example.com"}

	for _, sandbox := range []string{"", SandboxRunc, SandboxGvisor, SandboxKata} {
		req := &Request{ID: "app", Image: "nginx", Port: 8080, Sandbox: sandbox}
		s.applyDefaults(req)
		if err := s.validate(req); err != nil {
			t.Errorf("sandbox %q: want valid, got %v", sandbox, err)
		}
	}

	for _, sandbox := range []string{"firecracker", "Runc", "gVisor"} {
		req := &Request{ID: "app", Image: "nginx", Port: 8080, Sandbox: sandbox}
		s.applyDefaults(req)
		err := s.validate(req)
		if !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("sandbox %q: want validation error, got %v", sandbox, err)
		}
	}
}
