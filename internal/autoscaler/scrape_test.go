package autoscaler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSidecar serves /stats like the deployments-sidecar admin endpoint.
type fakeSidecar struct {
	server   *httptest.Server
	inFlight atomic.Int64
	requests atomic.Int64
}

func newFakeSidecar(t *testing.T) (*fakeSidecar, *url.URL, int) {
	t.Helper()
	f := &fakeSidecar{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"inFlight": f.inFlight.Load(),
			"requests": f.requests.Load(),
			"ready":    true,
		})
	}))
	t.Cleanup(f.server.Close)

	u, _ := url.Parse(f.server.URL)
	_, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)
	// The scraper derives the admin address from the data endpoint's host +
	// adminPort; using the same port makes the fake serve both roles.
	return f, u, port
}

type fixedEndpoints struct{ endpoints []*url.URL }

func (f fixedEndpoints) Endpoints(context.Context, string) ([]*url.URL, error) {
	return f.endpoints, nil
}

func TestScrapeActivity_CounterMovementMeansActive(t *testing.T) {
	sidecar, endpoint, port := newFakeSidecar(t)
	s := NewScrapeActivity(fixedEndpoints{[]*url.URL{endpoint}}, port)

	// First sight establishes a baseline and counts as activity.
	first, ok := s.LastActivity("web")
	if !ok {
		t.Fatal("expected activity after first scrape")
	}

	// No movement → lastActive must not advance.
	time.Sleep(10 * time.Millisecond)
	unchanged, _ := s.LastActivity("web")
	if unchanged.After(first) {
		t.Fatal("idle deployment reported as active")
	}

	// Counter movement → activity advances.
	sidecar.requests.Add(3)
	time.Sleep(10 * time.Millisecond)
	moved, _ := s.LastActivity("web")
	if !moved.After(first) {
		t.Fatal("request counter movement not detected as activity")
	}
}

func TestScrapeActivity_InFlightMeansActive(t *testing.T) {
	sidecar, endpoint, port := newFakeSidecar(t)
	s := NewScrapeActivity(fixedEndpoints{[]*url.URL{endpoint}}, port)

	first, _ := s.LastActivity("web")
	sidecar.inFlight.Store(1) // long-running request, counter static
	time.Sleep(10 * time.Millisecond)
	active, _ := s.LastActivity("web")
	if !active.After(first) {
		t.Fatal("in-flight request not detected as activity")
	}
}

func TestScrapeActivity_NoEndpointsReportsLastKnown(t *testing.T) {
	s := NewScrapeActivity(fixedEndpoints{nil}, 1)
	if _, ok := s.LastActivity("cold"); ok {
		t.Fatal("never-scraped deployment must report no activity")
	}
}
