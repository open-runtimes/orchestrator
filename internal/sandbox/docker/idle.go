package docker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"orchestrator/internal/proxy"
	"strconv"
	"time"
)

// The idle sweep is the Docker equivalent of the warm control loop's reaper:
// with an idle window declared, a sandbox whose request count has not moved
// across it is torn down. The counter is the sidecar's, read from its admin
// /stats, so what counts as activity is identical on both backends — including
// traffic to a sandbox's extra ports.
//
// Its marks are process-local, and there is exactly one deployments-service on
// Docker, so a restart simply restarts the clocks: a teardown is delayed by at
// most one window.

// idleMark remembers a sandbox's cumulative request count and when it last moved.
type idleMark struct {
	requests int64
	at       time.Time
}

// runReaper sweeps until ctx is cancelled.
func (o *Orchestrator) runReaper(ctx context.Context) {
	marks := make(map[string]idleMark)
	ticker := time.NewTicker(o.tick)
	defer ticker.Stop()
	for {
		o.reapIdle(ctx, marks)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reapIdle tears down every sandbox that has gone quiet for its window, and
// forgets the marks of sandboxes that no longer exist.
func (o *Orchestrator) reapIdle(ctx context.Context, marks map[string]idleMark) {
	volumes, err := o.volumes(ctx)
	if err != nil {
		slog.Warn("Sandbox idle sweep failed to list volumes", "error", err)
		return
	}
	live := make(map[string]bool, len(volumes))
	for _, vol := range volumes {
		id := vol.Labels[labelID]
		live[id] = true

		spec, err := parseSpec(vol.Labels[labelSpec])
		if err != nil || spec.IdleTimeoutSeconds <= 0 {
			continue
		}
		requests, ok := o.requestCount(ctx, id)
		if !ok {
			continue // not serving yet, or already gone
		}
		mark, seen := marks[id]
		if !seen || requests != mark.requests {
			marks[id] = idleMark{requests: requests, at: o.now()}
			continue
		}
		if o.now().Sub(mark.at) > time.Duration(spec.IdleTimeoutSeconds)*time.Second {
			slog.Info("Tearing down idle sandbox", "sandboxId", id)
			o.cleanup(ctx, id)
		}
	}
	for id := range marks {
		if !live[id] {
			delete(marks, id)
		}
	}
}

// requestCount reads the sidecar's cumulative accepted-request counter.
func (o *Orchestrator) requestCount(ctx context.Context, id string) (int64, bool) {
	info, err := o.inspect(ctx, proxyName(id))
	if err != nil || info == nil || info.State == nil || !info.State.Running {
		return 0, false
	}
	ip := containerIP(info.NetworkSettings, o.cfg.Network)
	if ip == "" {
		return 0, false
	}
	statsURL := "http://" + net.JoinHostPort(ip, strconv.Itoa(proxy.DefaultAdminPort)) + "/stats"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, nil)
	if err != nil {
		return 0, false
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var stats struct {
		Requests int64 `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return 0, false
	}
	return stats.Requests, true
}
