package kube

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// LeaderElectionConfig controls how replicas coordinate so that exactly one
// of them runs a leader-gated component (e.g. a lifecycle watcher). HTTP
// reads/writes are always handled by any replica — only the component is
// leader-gated.
type LeaderElectionConfig struct {
	Enabled       bool          // when false, RunLeaderElected runs the component directly (single-replica mode)
	LeaseName     string        // Lease resource name in the configured namespace
	Identity      string        // unique per-replica string (usually Pod name)
	LeaseDuration time.Duration // how long non-leaders wait before taking over after a failed renewal
	RenewDeadline time.Duration // how long the leader retries renewing before giving up
	RetryPeriod   time.Duration // how often non-leaders try to acquire the lease
}

// ApplyDefaults fills in sensible defaults for leader-election timing and
// identity, matching the norms from K8s itself (15s/10s/2s).
func (cfg *LeaderElectionConfig) ApplyDefaults(defaultLeaseName string) {
	if cfg.LeaseName == "" {
		cfg.LeaseName = defaultLeaseName
	}
	if cfg.Identity == "" {
		if hn, err := os.Hostname(); err == nil {
			cfg.Identity = hn
		} else {
			cfg.Identity = fmt.Sprintf("unknown-%d", time.Now().UnixNano())
		}
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 15 * time.Second
	}
	if cfg.RenewDeadline <= 0 {
		cfg.RenewDeadline = 10 * time.Second
	}
	if cfg.RetryPeriod <= 0 {
		cfg.RetryPeriod = 2 * time.Second
	}
}

// RunLeaderElected runs `run` while this process holds the lease, looping so
// that if the lease is lost the process re-competes, until ctx cancels. With
// Enabled=false (single-replica mode) it runs `run` directly. onLeadership,
// when non-nil, is called as leadership is gained/lost — with the identity
// label to record against.
func RunLeaderElected(ctx context.Context, client kubernetes.Interface, namespace string, cfg LeaderElectionConfig, run func(context.Context), onLeadership func(context.Context, string, bool)) {
	notify := func(ctx context.Context, identity string, leading bool) {
		if onLeadership != nil {
			onLeadership(ctx, identity, leading)
		}
	}

	if !cfg.Enabled {
		// Single-replica mode: this process is effectively always the leader,
		// so report it as such. identity is the hostname (or pod name) so
		// dashboards still have a label to display.
		identity := cfg.Identity
		if identity == "" {
			identity, _ = os.Hostname()
		}
		if identity == "" {
			identity = "single-replica"
		}
		notify(ctx, identity, true)
		run(ctx)
		notify(context.Background(), identity, false)
		return
	}

	logger := slog.With("component", "k8s.leaderelection", "identity", cfg.Identity)
	for {
		if ctx.Err() != nil {
			return
		}
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      cfg.LeaseName,
				Namespace: namespace,
			},
			Client: client.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: cfg.Identity,
			},
		}
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:            lock,
			ReleaseOnCancel: true,
			LeaseDuration:   cfg.LeaseDuration,
			RenewDeadline:   cfg.RenewDeadline,
			RetryPeriod:     cfg.RetryPeriod,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(leaderCtx context.Context) {
					logger.Info("Acquired leadership; starting leader-gated component")
					notify(leaderCtx, cfg.Identity, true)
					run(leaderCtx)
					logger.Info("Leader-gated component stopped; leadership term ended")
				},
				OnStoppedLeading: func() {
					logger.Info("Lost leadership")
					notify(context.Background(), cfg.Identity, false)
				},
				OnNewLeader: func(identity string) {
					if identity != cfg.Identity {
						logger.Info("New leader elected", "leader", identity)
					}
				},
			},
		})
	}
}
