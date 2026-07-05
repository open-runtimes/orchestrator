// Package autoscaler is the deployments scaling control loop — a tiny KPA:
//
//	desired = clamp(ceil(avgConcurrencyOverWindow / target), minReplicas, maxReplicas)
//
// It ticks fast and smooths slow (the window is a smoothing horizon, not the
// reaction time), owns 1↔N and N→0, and never fights the activator, which
// owns 0→N on a cold hit: while requests queue in the activator the queued
// count holds the deployment up. See docs/design/deployments-autoscaler.md.
package autoscaler

import (
	"context"
	"log/slog"
	"math"
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

// ConcurrencySource reports a deployment's current in-flight request count
// summed across its replicas (sidecar /stats scrape).
type ConcurrencySource interface {
	Concurrency(ctx context.Context, id string) (float64, error)
}

// QueueSource reports how many requests are waiting in the activator for a
// deployment — the hold-up signal while a cold start is in flight, when there
// are no sidecars to scrape.
type QueueSource interface {
	Queued(ctx context.Context, id string) int
}

// Config controls the loop cadence.
type Config struct {
	Tick   time.Duration // evaluation interval
	Window time.Duration // smoothing horizon for the concurrency average
}

// LoadConfigFromEnv loads loop configuration from the environment.
func LoadConfigFromEnv() Config {
	return Config{
		Tick:   config.GetDurationEnv("AUTOSCALER_TICK", 2*time.Second),
		Window: config.GetDurationEnv("AUTOSCALER_WINDOW", 60*time.Second),
	}
}

// Autoscaler drives replica counts for deployments with autoscaling set.
type Autoscaler struct {
	backend     Backend
	concurrency ConcurrencySource
	queue       QueueSource
	cfg         Config

	windows map[string]*window
}

// New creates the loop. Call Run to start it (leader-gate it on Kubernetes —
// exactly one replica may write scales).
func New(backend Backend, concurrency ConcurrencySource, queue QueueSource, cfg Config) *Autoscaler {
	return &Autoscaler{
		backend:     backend,
		concurrency: concurrency,
		queue:       queue,
		cfg:         cfg,
		windows:     make(map[string]*window),
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

// evaluate samples every autoscaled deployment and reconciles its replica
// count. All state except the sample windows derives from the backend.
func (a *Autoscaler) evaluate(ctx context.Context) {
	statuses, err := a.backend.List(ctx)
	if err != nil {
		slog.Warn("Autoscaler evaluation skipped", "error", err)
		return
	}

	now := time.Now()
	seen := make(map[string]bool, len(statuses))
	for i := range statuses {
		status := &statuses[i]
		seen[status.ID] = true
		a.evaluateOne(ctx, now, status)
	}
	for id := range a.windows {
		if !seen[id] {
			delete(a.windows, id)
		}
	}
}

func (a *Autoscaler) evaluateOne(ctx context.Context, now time.Time, status *deployment.StatusResponse) {
	spec, err := a.backend.Spec(ctx, status.ID)
	if err != nil || spec.Autoscaling == nil {
		delete(a.windows, status.ID)
		return
	}

	sample, err := a.concurrency.Concurrency(ctx, status.ID)
	if err != nil {
		sample = 0 // cold or unready — the queue signal carries the load
	}
	queued := a.queue.Queued(ctx, status.ID)
	sample += float64(queued)

	w := a.windows[status.ID]
	if w == nil {
		w = &window{firstSeen: now}
		a.windows[status.ID] = w
	}
	w.push(now, sample, a.cfg.Window)

	auto := spec.Autoscaling
	desired := int(math.Ceil(w.average() / float64(auto.Target)))
	desired = min(max(desired, auto.MinReplicas), auto.MaxReplicas)
	if queued > 0 {
		// Requests are waiting in the activator: never conclude zero.
		desired = max(desired, 1)
	}

	current := status.DesiredReplicas
	if desired == current {
		return
	}
	// Scale-down decisions need a full window of evidence; a freshly observed
	// deployment gets that grace before its lack of history reads as idle.
	if desired < current && now.Sub(w.firstSeen) < a.cfg.Window {
		return
	}

	if err := a.backend.Scale(ctx, status.ID, desired); err != nil {
		slog.Warn("Autoscale failed", "deploymentId", status.ID, "desired", desired, "error", err)
		return
	}
	slog.Info("Autoscaled", "deploymentId", status.ID, "from", current, "to", desired,
		"avgConcurrency", math.Round(w.average()*10)/10, "queued", queued)
}

// window is a per-deployment sliding sample window.
type window struct {
	firstSeen time.Time
	samples   []sample
}

type sample struct {
	at    time.Time
	value float64
}

func (w *window) push(now time.Time, value float64, span time.Duration) {
	w.samples = append(w.samples, sample{at: now, value: value})
	cutoff := now.Add(-span)
	i := 0
	for i < len(w.samples) && w.samples[i].at.Before(cutoff) {
		i++
	}
	w.samples = w.samples[i:]
}

func (w *window) average() float64 {
	if len(w.samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range w.samples {
		sum += s.value
	}
	return sum / float64(len(w.samples))
}
