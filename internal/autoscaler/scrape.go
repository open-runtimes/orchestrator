package autoscaler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// EndpointLister supplies the ready proxy endpoints to scrape. Satisfied by
// the deployment orchestrator.
type EndpointLister interface {
	Endpoints(ctx context.Context, id string) ([]*url.URL, error)
}

// ScrapeActivity derives per-deployment activity from the deployments-sidecar
// /stats endpoint: in-flight requests or a moving cumulative request counter
// mean active. It replaces the activator as the activity source once warm
// traffic is off the activator's path (Phase 3, gateway data plane).
type ScrapeActivity struct {
	endpoints EndpointLister
	adminPort int
	client    *http.Client

	mu   sync.Mutex
	seen map[string]scrapeState
}

type scrapeState struct {
	requests   int64
	lastActive time.Time
}

// sidecarStats mirrors the deployments-sidecar /stats payload.
type sidecarStats struct {
	InFlight int64 `json:"inFlight"`
	Requests int64 `json:"requests"`
}

// NewScrapeActivity creates the scraping activity source. adminPort is the
// sidecar admin port (proxy.DefaultAdminPort).
func NewScrapeActivity(endpoints EndpointLister, adminPort int) *ScrapeActivity {
	return &ScrapeActivity{
		endpoints: endpoints,
		adminPort: adminPort,
		client:    &http.Client{Timeout: 2 * time.Second},
		seen:      make(map[string]scrapeState),
	}
}

// LastActivity scrapes the deployment's sidecars and reports the last time
// traffic was observed. Called once per deployment per evaluation tick, so
// the scrape happens at the idle loop's cadence.
func (s *ScrapeActivity) LastActivity(id string) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	endpoints, err := s.endpoints.Endpoints(ctx, id)
	if err != nil || len(endpoints) == 0 {
		// Nothing to scrape (cold or unready) — report what we knew last.
		s.mu.Lock()
		defer s.mu.Unlock()
		state, ok := s.seen[id]
		return state.lastActive, ok
	}

	var inFlight, requests int64
	for _, endpoint := range endpoints {
		stats, err := s.scrape(ctx, endpoint)
		if err != nil {
			continue
		}
		inFlight += stats.InFlight
		requests += stats.Requests
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state, known := s.seen[id]
	active := inFlight > 0 || !known || requests != state.requests
	if active {
		state.lastActive = time.Now()
	}
	state.requests = requests
	s.seen[id] = state
	return state.lastActive, true
}

// scrape fetches one sidecar's /stats via its admin port.
func (s *ScrapeActivity) scrape(ctx context.Context, endpoint *url.URL) (*sidecarStats, error) {
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
