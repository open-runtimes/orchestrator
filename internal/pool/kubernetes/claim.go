package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"orchestrator/internal/proxy"
	"strconv"
	"strings"
	"time"
)

// errClaimConflict is the racing loser's result: the pod's sidecar already
// accepted another activation. The caller retries the next warm pod.
var errClaimConflict = errors.New("pod already claimed")

// claimClient is the sidecar-facing HTTP surface — the claim POST plus the
// probes the control loops rely on. Factored behind an interface so unit
// tests can fake pods' sidecars: fake-clientset pods have no reachable IPs.
type claimClient interface {
	// Claim POSTs the activation to the pod's sidecar with the pod's bearer
	// token. 409 → errClaimConflict.
	Claim(ctx context.Context, podIP, token string, req *proxy.ClaimRequest) error
	// State reads the sidecar's authoritative claim record — the poison and
	// orphan-GC source of truth.
	State(ctx context.Context, podIP string) (*proxy.ClaimState, error)
	// Ready reports the sidecar's /ready gate: warm-ready before a claim,
	// serving-ready after.
	Ready(ctx context.Context, podIP string) bool
	// Requests reads the sidecar's cumulative accepted-request counter — the
	// idle-teardown signal (a zero delta across a window means idle).
	Requests(ctx context.Context, podIP string) (int64, error)
}

// httpClaimClient talks to sidecar admin ports directly by pod IP.
type httpClaimClient struct {
	client *http.Client
}

func newClaimClient() *httpClaimClient {
	return &httpClaimClient{client: &http.Client{Timeout: 10 * time.Second}}
}

// adminURL addresses the sidecar admin port on a pod.
func adminURL(podIP, path string) string {
	return "http://" + net.JoinHostPort(podIP, strconv.Itoa(proxy.DefaultAdminPort)) + path
}

func (c *httpClaimClient) Claim(ctx context.Context, podIP, token string, req *proxy.ClaimRequest) error {
	// ClaimRequest's custom codec routes artifacts through the registry so
	// each carries its "type" discriminator.
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adminURL(podIP, proxy.ClaimPath), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusConflict:
		return errClaimConflict
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("claim rejected with status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

func (c *httpClaimClient) State(ctx context.Context, podIP string) (*proxy.ClaimState, error) {
	var state proxy.ClaimState
	if err := c.getJSON(ctx, adminURL(podIP, proxy.ClaimStatePath), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (c *httpClaimClient) Ready(ctx context.Context, podIP string) bool {
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

func (c *httpClaimClient) Requests(ctx context.Context, podIP string) (int64, error) {
	var stats struct {
		Requests int64 `json:"requests"`
	}
	if err := c.getJSON(ctx, adminURL(podIP, "/stats"), &stats); err != nil {
		return 0, err
	}
	return stats.Requests, nil
}

func (c *httpClaimClient) getJSON(ctx context.Context, url string, out any) error {
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
