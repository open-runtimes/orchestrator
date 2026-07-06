package autoscaler

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// maxScrapeBody bounds any scraped /stats response — a misbehaving sidecar
// or activator must not be able to balloon the autoscaler's memory.
const maxScrapeBody = 64 << 10 // 64 KiB

// EndpointLister supplies the ready proxy endpoints to scrape. Satisfied by
// the deployment orchestrator.
type EndpointLister interface {
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)
}

// SidecarConcurrency derives each replica's average concurrency from the
// deployments-sidecar /stats concurrency-seconds integral: Δintegral/Δt
// between our own scrapes. Instantaneous in-flight sampling under-reads fast
// handlers (a 2s tick sees ~0 between millisecond requests); the integral
// measures the true time-averaged load. First sight of a pod falls back to
// its instantaneous in-flight.
type SidecarConcurrency struct {
	endpoints EndpointLister
	adminPort int
	client    *http.Client

	mu   sync.Mutex
	prev map[string]integralSample // endpoint host → last scrape
}

type integralSample struct {
	integral float64
	at       time.Time
}

// sidecarStats mirrors the deployments-sidecar /stats payload.
type sidecarStats struct {
	InFlight           int64   `json:"inFlight"`
	ConcurrencySeconds float64 `json:"concurrencySeconds"`
}

// NewSidecarConcurrency creates the scraper. adminPort is the sidecar admin
// port (proxy.DefaultAdminPort).
func NewSidecarConcurrency(endpoints EndpointLister, adminPort int) *SidecarConcurrency {
	return &SidecarConcurrency{
		endpoints: endpoints,
		adminPort: adminPort,
		client:    &http.Client{Timeout: 2 * time.Second},
		prev:      make(map[string]integralSample),
	}
}

// Concurrency sums average concurrency across the deployment's ready replicas.
func (s *SidecarConcurrency) Concurrency(ctx context.Context, id string) (float64, error) {
	endpoints, err := s.endpoints.Endpoints(ctx, id)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	var total float64
	for _, endpoint := range endpoints {
		stats, err := s.scrape(ctx, endpoint)
		if err != nil {
			continue // a churning pod mid-scrape is not an evaluation failure
		}
		total += s.rate(endpoint.Host, stats, now)
	}
	return total, nil
}

// rate converts one replica's integral into its average concurrency since our
// previous scrape. A shrinking integral means the pod restarted — treat as
// first sight.
func (s *SidecarConcurrency) rate(host string, stats *sidecarStats, now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, seen := s.prev[host]
	s.prev[host] = integralSample{integral: stats.ConcurrencySeconds, at: now}

	elapsed := now.Sub(prev.at).Seconds()
	if !seen || stats.ConcurrencySeconds < prev.integral || elapsed <= 0 {
		return float64(stats.InFlight)
	}
	return (stats.ConcurrencySeconds - prev.integral) / elapsed
}

func (s *SidecarConcurrency) scrape(ctx context.Context, endpoint *url.URL) (*sidecarStats, error) {
	host, _, err := net.SplitHostPort(endpoint.Host)
	if err != nil {
		host = endpoint.Host
	}
	statsURL := "http://" + net.JoinHostPort(host, strconv.Itoa(s.adminPort)) + "/stats"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var stats sidecarStats
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxScrapeBody)).Decode(&stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

// ActivatorQueue scrapes the standalone deployments-activator's /stats for
// queued-per-revision counts and rolls them up per deployment (revision names
// are {id}-NNNNN). The Kubernetes queue source; on Docker the in-process
// activator is queried directly. One /stats payload covers every deployment,
// so the fetch is memoized for the evaluation tick rather than repeated per
// deployment.
type ActivatorQueue struct {
	statsURL string
	client   *http.Client

	mu        sync.Mutex
	queued    map[string]int
	fetchedAt time.Time
}

// activatorStatsTTL memoizes the /stats payload across one autoscaler tick
// (2s default): N autoscaled deployments must not mean N fetches per tick.
const activatorStatsTTL = time.Second

// NewActivatorQueue creates the scraper (e.g. http://deployments-activator:8081/stats).
func NewActivatorQueue(statsURL string) *ActivatorQueue {
	return &ActivatorQueue{statsURL: statsURL, client: &http.Client{Timeout: 2 * time.Second}}
}

// revisionSuffix strips the revision counter off a revision name — %05d, so
// five digits that grow past 99999 without a separator change.
var revisionSuffix = regexp.MustCompile(`-\d{5,}$`)

// Queued sums the activator's queued requests across the deployment's
// revisions. Scrape failures read as zero — the concurrency average still
// governs, and a missing hold-up signal only risks a premature scale-down
// that the next cold hit re-raises.
func (q *ActivatorQueue) Queued(ctx context.Context, id string) int {
	queued := q.snapshot(ctx)
	total := 0
	for rev, n := range queued {
		if revisionSuffix.ReplaceAllString(rev, "") == id {
			total += n
		}
	}
	return total
}

// snapshot returns the queued-per-revision map, fetching at most once per TTL.
func (q *ActivatorQueue) snapshot(ctx context.Context) map[string]int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if time.Since(q.fetchedAt) < activatorStatsTTL {
		return q.queued
	}
	q.fetchedAt = time.Now()
	q.queued = nil

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.statsURL, nil)
	if err != nil {
		return nil
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var payload struct {
		Queued map[string]int `json:"queued"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxScrapeBody)).Decode(&payload); err != nil {
		return nil
	}
	q.queued = payload.Queued
	return q.queued
}

// QueuedDepthFunc adapts a direct in-process lookup (the Docker activator's
// QueuedDepth method) into a QueueSource.
type QueuedDepthFunc func(id string) int

// Queued implements QueueSource.
func (f QueuedDepthFunc) Queued(_ context.Context, id string) int { return f(id) }
