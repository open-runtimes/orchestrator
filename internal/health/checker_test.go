package health

import (
	"context"
	"testing"
)

func TestChecker_Liveness(t *testing.T) {
	t.Parallel()
	checker := NewChecker(nil)

	response := checker.Liveness(context.Background())

	if response.Status != StatusHealthy {
		t.Errorf("Expected healthy status, got %s", response.Status)
	}
}

func TestChecker_Readiness_NoOrchestrator(t *testing.T) {
	t.Parallel()
	checker := NewChecker(nil)

	response := checker.Readiness(context.Background())

	if response.Status != StatusUnhealthy {
		t.Errorf("Expected unhealthy status, got %s", response.Status)
	}

	if response.Checks == nil {
		t.Fatal("Expected checks to be present")
	}

	orchestratorCheck, ok := response.Checks["orchestrator"]
	if !ok {
		t.Fatal("Expected orchestrator check to be present")
	}

	if orchestratorCheck.Status != StatusUnhealthy {
		t.Errorf("Expected orchestrator check to be unhealthy, got %s", orchestratorCheck.Status)
	}
}

func TestResponse_IsHealthy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		status   Status
		expected bool
	}{
		{"healthy", StatusHealthy, true},
		{"unhealthy", StatusUnhealthy, false},
		{"degraded", StatusDegraded, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			response := &Response{Status: tt.status}
			if response.IsHealthy() != tt.expected {
				t.Errorf("IsHealthy() = %v, want %v", response.IsHealthy(), tt.expected)
			}
		})
	}
}
