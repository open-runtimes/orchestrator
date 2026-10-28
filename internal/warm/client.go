package warm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/claim"
	"orchestrator/internal/config"
	"orchestrator/internal/workload"
	"strconv"
	"time"
)

// Client is the sidecar-facing HTTP surface — the claim POST plus the probes
// the control loops and status derivation rely on. Factored behind an
// interface so unit tests can fake pods' sidecars: fake-clientset pods have no
// reachable IPs.
type Client interface {
	// Claim POSTs the payload to the pod's sidecar with the pod's bearer
	// token. 409 → claim.ErrConflict; 422 → *claim.Poison.
	Claim(ctx context.Context, podIP, token string, req *workload.ClaimRequest) error
	// State reads the sidecar's authoritative claim record — the poison and
	// orphan-GC source of truth.
	State(ctx context.Context, podIP string) (*workload.ClaimState, error)
	// Ready reports the sidecar's /ready gate: warm-ready before a claim,
	// serving-ready after.
	Ready(ctx context.Context, podIP string) bool
	// Requests reads the sidecar's cumulative accepted-request counter — the
	// idle-teardown signal (a zero delta across a window means idle).
	Requests(ctx context.Context, podIP string) (int64, error)
}

// httpClient talks to sidecar admin ports directly by pod IP.
type httpClient struct {
	client *http.Client
	poster claim.Poster
	s3     config.S3Profiles // mints each claim's S3 grants; the pod never sees them
}

func newHTTPClient() *httpClient {
	return &httpClient{
		client: &http.Client{Timeout: 10 * time.Second},
		poster: claim.NewPoster(),
		s3:     config.LoadS3Profiles(),
	}
}

// AdminURL addresses the sidecar admin port on a pod.
func adminURL(podIP, path string) string {
	return "http://" + net.JoinHostPort(podIP, strconv.Itoa(workload.DefaultAdminPort)) + path
}

func (c *httpClient) Claim(ctx context.Context, podIP, token string, req *workload.ClaimRequest) error {
	granted := *req
	granted.S3Grants = artifact.GrantsFor(req.Artifacts, c.s3)
	return c.poster.Post(ctx, claim.Unit{Addr: podIP, Token: token}, &granted)
}

func (c *httpClient) State(ctx context.Context, podIP string) (*workload.ClaimState, error) {
	var state workload.ClaimState
	if err := c.getJSON(ctx, adminURL(podIP, workload.ClaimStatePath), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (c *httpClient) Ready(ctx context.Context, podIP string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminURL(podIP, "/ready"), nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (c *httpClient) Requests(ctx context.Context, podIP string) (int64, error) {
	var stats struct {
		Requests int64 `json:"requests"`
	}
	if err := c.getJSON(ctx, adminURL(podIP, "/stats"), &stats); err != nil {
		return 0, err
	}
	return stats.Requests, nil
}

func (c *httpClient) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
