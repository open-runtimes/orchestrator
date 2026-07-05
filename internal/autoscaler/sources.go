package autoscaler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// EndpointLister supplies the ready proxy endpoints to scrape. Satisfied by
// the deployment orchestrator.
type EndpointLister interface {
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)
}

// SidecarConcurrency sums in-flight requests across a deployment's
// deployments-sidecar /stats endpoints — the warm-traffic metric source (the
// sidecar is the metering point once warm traffic is off the activator path).
type SidecarConcurrency struct {
	endpoints EndpointLister
	adminPort int
	client    *http.Client
}

// sidecarStats mirrors the deployments-sidecar /stats payload.
type sidecarStats struct {
	InFlight int64 `json:"inFlight"`
}

// NewSidecarConcurrency creates the scraper. adminPort is the sidecar admin
// port (proxy.DefaultAdminPort).
func NewSidecarConcurrency(endpoints EndpointLister, adminPort int) *SidecarConcurrency {
	return &SidecarConcurrency{
		endpoints: endpoints,
		adminPort: adminPort,
		client:    &http.Client{Timeout: 2 * time.Second},
	}
}

// Concurrency sums in-flight requests across the deployment's ready replicas.
func (s *SidecarConcurrency) Concurrency(ctx context.Context, id string) (float64, error) {
	endpoints, err := s.endpoints.Endpoints(ctx, id)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, endpoint := range endpoints {
		stats, err := s.scrape(ctx, endpoint)
		if err != nil {
			continue // a churning pod mid-scrape is not an evaluation failure
		}
		total += float64(stats.InFlight)
	}
	return total, nil
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
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return nil, err
	}
	return &stats, nil
}

// ActivatorQueue scrapes the standalone deployments-activator's /stats for
// queued-per-revision counts and rolls them up per deployment (revision names
// are {id}-NNNNN). The Kubernetes queue source; on Docker the in-process
// activator is queried directly.
type ActivatorQueue struct {
	statsURL string
	client   *http.Client
}

// NewActivatorQueue creates the scraper (e.g. http://deployments-activator:8081/stats).
func NewActivatorQueue(statsURL string) *ActivatorQueue {
	return &ActivatorQueue{statsURL: statsURL, client: &http.Client{Timeout: 2 * time.Second}}
}

// revisionSuffix strips the -NNNNN revision counter off a revision name.
var revisionSuffix = regexp.MustCompile(`-\d{5}$`)

// Queued sums the activator's queued requests across the deployment's
// revisions. Scrape failures read as zero — the concurrency average still
// governs, and a missing hold-up signal only risks a premature scale-down
// that the next cold hit re-raises.
func (q *ActivatorQueue) Queued(ctx context.Context, id string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.statsURL, nil)
	if err != nil {
		return 0
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	var payload struct {
		Queued map[string]int `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0
	}
	total := 0
	for rev, n := range payload.Queued {
		if revisionSuffix.ReplaceAllString(rev, "") == id {
			total += n
		}
	}
	return total
}

// QueuedDepthFunc adapts a direct in-process lookup (the Docker activator's
// QueuedDepth method) into a QueueSource.
type QueuedDepthFunc func(id string) int

// Queued implements QueueSource.
func (f QueuedDepthFunc) Queued(_ context.Context, id string) int { return f(id) }
