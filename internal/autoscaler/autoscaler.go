// Package autoscaler is the deployments scaling control loop. In Phase 2 it
// only returns idle deployments to zero; concurrency-driven 1↔N (the tiny KPA
// of docs/design/deployments-autoscaler.md) grows here in Phase 4. The
// activator owns the opposite direction — 0→N on a cold hit — so the two
// writers can't fight: this loop only ever writes zero.
package autoscaler

import (
	"context"
	"log/slog"
	"orchestrator/internal/config"
	"orchestrator/pkg/deployment"
	"time"
)

// Backend is the slice of deployment.Orchestrator this loop needs.
type Backend interface {
	List(ctx context.Context) ([]deployment.StatusResponse, error)
	Spec(ctx context.Context, id string) (*deployment.Request, error)
	Scale(ctx context.Context, id string, replicas int) error
}

// ActivitySource reports when a deployment last received a request. While the
// activator is on the request path for all traffic (pre-gateway), it is this
// source; Phase 4 replaces it with sidecar concurrency scrapes.
type ActivitySource interface {
	LastActivity(id string) (time.Time, bool)
}

// Config controls the loop cadence.
type Config struct {
	Window time.Duration // idle this long → scale to zero
	Tick   time.Duration // evaluation interval
}

// LoadConfigFromEnv loads loop configuration from the environment.
func LoadConfigFromEnv() Config {
	return Config{
		Window: config.GetDurationEnv("SCALE_TO_ZERO_WINDOW", 60*time.Second),
		Tick:   config.GetDurationEnv("SCALE_TO_ZERO_TICK", 5*time.Second),
	}
}

// Autoscaler scales idle deployments with autoscaling.minReplicas: 0 down to
// zero after Config.Window without a request.
type Autoscaler struct {
	backend  Backend
	activity ActivitySource
	cfg      Config

	// firstSeen baselines deployments that have never received a request
	// since this process started: each gets a full window from first
	// observation before it may be zeroed.
	firstSeen map[string]time.Time
}

// New creates the loop. Call Run to start it.
func New(backend Backend, activity ActivitySource, cfg Config) *Autoscaler {
	return &Autoscaler{
		backend:   backend,
		activity:  activity,
		cfg:       cfg,
		firstSeen: make(map[string]time.Time),
	}
}

// Run evaluates every Config.Tick until ctx cancels.
func (a *Autoscaler) Run(ctx context.Context) {
	ticker := time.NewTicker(a.cfg.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.evaluate(ctx)
		}
	}
}

// evaluate scales every idle-eligible deployment to zero once its window
// expires. All state derives from the backend; the loop itself only holds the
// first-seen baseline.
func (a *Autoscaler) evaluate(ctx context.Context) {
	statuses, err := a.backend.List(ctx)
	if err != nil {
		slog.Warn("Idle-to-zero evaluation skipped", "error", err)
		return
	}

	now := time.Now()
	seen := make(map[string]bool, len(statuses))
	for i := range statuses {
		status := &statuses[i]
		seen[status.ID] = true
		if _, ok := a.firstSeen[status.ID]; !ok {
			a.firstSeen[status.ID] = now
		}
		if status.DesiredReplicas == 0 {
			continue // already at zero
		}

		spec, err := a.backend.Spec(ctx, status.ID)
		if err != nil || spec.Autoscaling == nil || spec.Autoscaling.MinReplicas != 0 {
			continue // not opted into scale-to-zero
		}

		last, ok := a.activity.LastActivity(status.ID)
		if !ok {
			last = a.firstSeen[status.ID]
		}
		if now.Sub(last) < a.cfg.Window {
			continue
		}

		if err := a.backend.Scale(ctx, status.ID, 0); err != nil {
			slog.Warn("Idle-to-zero scale-down failed", "deploymentId", status.ID, "error", err)
			continue
		}
		slog.Info("Scaled idle deployment to zero", "deploymentId", status.ID, "idle", now.Sub(last).Round(time.Second))
	}

	// Prune baselines for deleted deployments.
	for id := range a.firstSeen {
		if !seen[id] {
			delete(a.firstSeen, id)
		}
	}
}
