// Package health provides health check functionality for liveness and readiness probes.
package health

import (
	"context"
	"sync"
	"time"
)

// ReadinessChecker is the interface for readiness checks.
// Implemented by orchestrators to verify they are ready to accept work.
type ReadinessChecker interface {
	Ready(ctx context.Context) error
}

// Status represents the health status of a component.
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusDegraded  Status = "degraded"
)

// CheckResult contains the result of a health check.
type CheckResult struct {
	Status  Status `json:"status"`
	Message string `json:"message,omitempty"`
}

// Response is the health check response.
type Response struct {
	Status Status                 `json:"status"`
	Checks map[string]CheckResult `json:"checks,omitempty"`
}

// Checker performs health checks on dependencies.
type Checker struct {
	orchestrators []ReadinessChecker
	timeout       time.Duration

	mu             sync.RWMutex
	lastCheck      time.Time
	cachedReady    *Response
	isShuttingDown bool
}

// NewChecker creates a new health checker. It takes more than one orchestrator
// for the all-in-one binary, where several control planes share one /readyz:
// the service is ready only when every one of them is.
func NewChecker(orchestrators ...ReadinessChecker) *Checker {
	live := make([]ReadinessChecker, 0, len(orchestrators))
	for _, o := range orchestrators {
		if o != nil {
			live = append(live, o)
		}
	}
	return &Checker{
		orchestrators: live,
		timeout:       5 * time.Second,
	}
}

// Liveness returns true if the service is alive.
// This should be a lightweight check that doesn't depend on external services.
// Failing this probe should trigger a container restart.
func (c *Checker) Liveness(ctx context.Context) *Response {
	return &Response{
		Status: StatusHealthy,
	}
}

// Readiness checks if the service is ready to accept traffic.
// This checks all dependencies (Docker daemon).
// Failing this probe should remove the instance from load balancer rotation.
func (c *Checker) Readiness(ctx context.Context) *Response {
	c.mu.RLock()
	// Return unhealthy immediately if shutting down
	if c.isShuttingDown {
		c.mu.RUnlock()
		return &Response{
			Status: StatusUnhealthy,
			Checks: map[string]CheckResult{
				"shutdown": {Status: StatusUnhealthy, Message: "service is shutting down"},
			},
		}
	}

	// Use cached result if recent (avoid hammering Docker)
	if c.cachedReady != nil && time.Since(c.lastCheck) < time.Second {
		cached := c.cachedReady
		c.mu.RUnlock()
		return cached
	}
	c.mu.RUnlock()

	checks := make(map[string]CheckResult)
	overallStatus := StatusHealthy

	// Check orchestrator backend
	orchestratorCheck := c.checkOrchestrator(ctx)
	checks["orchestrator"] = orchestratorCheck
	if orchestratorCheck.Status != StatusHealthy {
		overallStatus = StatusUnhealthy
	}

	response := &Response{
		Status: overallStatus,
		Checks: checks,
	}

	// Cache the result
	c.mu.Lock()
	c.cachedReady = response
	c.lastCheck = time.Now()
	c.mu.Unlock()

	return response
}

// checkOrchestrator verifies every orchestrator is ready to accept work. The
// first one that is not decides the result: a plane that cannot serve makes the
// whole process unready, which is what a shared readiness probe has to mean.
func (c *Checker) checkOrchestrator(ctx context.Context) CheckResult {
	if len(c.orchestrators) == 0 {
		return CheckResult{
			Status:  StatusUnhealthy,
			Message: "orchestrator not configured",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	for _, orchestrator := range c.orchestrators {
		if err := orchestrator.Ready(ctx); err != nil {
			return CheckResult{
				Status:  StatusUnhealthy,
				Message: err.Error(),
			}
		}
	}

	return CheckResult{
		Status: StatusHealthy,
	}
}

// IsHealthy returns true if the overall status is healthy.
func (r *Response) IsHealthy() bool {
	return r.Status == StatusHealthy
}

// SetShuttingDown marks the service as shutting down.
// This causes readiness checks to return unhealthy, signaling
// load balancers to stop sending new traffic.
func (c *Checker) SetShuttingDown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.isShuttingDown = true
	c.cachedReady = nil // Clear cache to ensure immediate effect
}
