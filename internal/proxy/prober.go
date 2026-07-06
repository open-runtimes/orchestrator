package proxy

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// prober tracks user-container readiness. It starts not-ready; a single probe
// success flips ready, and threshold consecutive failures flip it back.
type prober struct {
	target    string
	path      string // empty = TCP connect instead of HTTP GET
	period    time.Duration
	timeout   time.Duration
	threshold int

	client   *http.Client
	dialer   *net.Dialer
	ready    atomic.Bool
	failures int // consecutive; only touched by the run goroutine
}

func newProber(cfg Config) *prober {
	return &prober{
		target:    cfg.Target,
		path:      cfg.ReadinessPath,
		period:    cfg.ReadinessPeriod,
		timeout:   cfg.ReadinessTimeout,
		threshold: cfg.ReadinessFailureThreshold,
		client:    &http.Client{Timeout: cfg.ReadinessTimeout},
		dialer:    &net.Dialer{Timeout: cfg.ReadinessTimeout},
	}
}

// Ready reports the current readiness verdict.
func (pr *prober) Ready() bool { return pr.ready.Load() }

func (pr *prober) run(ctx context.Context) {
	ticker := time.NewTicker(pr.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pr.record(pr.probe(ctx))
		}
	}
}

func (pr *prober) record(success bool) {
	if success {
		pr.failures = 0
		pr.ready.Store(true)
		return
	}
	pr.failures++
	if pr.failures >= pr.threshold {
		pr.ready.Store(false)
	}
}

func (pr *prober) probe(ctx context.Context) bool {
	if pr.path == "" {
		conn, err := pr.dialer.DialContext(ctx, "tcp", pr.target)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}

	ctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+pr.target+pr.path, nil)
	if err != nil {
		return false
	}
	resp, err := pr.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusBadRequest
}
