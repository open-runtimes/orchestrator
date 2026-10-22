// Package mountagent is the contract with the node mount agent
// (docs/design/node-mount-agent.md): the names the mappers and the agent
// must agree on, the wire types, and the Client the sidecar uses as its
// Mounter. The agent itself is internal/mountagent/node; this package stays
// free of it, so the services and the sidecar do not link the CSI server.
package mountagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"orchestrator/internal/mount"
	"strings"
	"time"
)

const (
	// Driver is the agent's CSI driver name.
	Driver = "mount.open-runtimes.dev"
	// DefaultPort is where the agent's API listens on each node.
	DefaultPort = 9878
	// AttributeToken is the workspace volume attribute carrying the pod's
	// token. Each pod gets its own (kube.AgentWorkspace, kube.StampAgentToken),
	// and its sidecar the same value (EnvToken); presenting it authorizes
	// mounts in that pod's workspace and nowhere else. The worker sees neither.
	AttributeToken = "token"
	// EnvEndpoint and EnvToken are how the sidecar finds and authenticates to
	// the agent on its node. With a token set the sidecar uses the agent.
	EnvEndpoint = "MOUNT_AGENT_ENDPOINT"
	EnvToken    = "MOUNT_AGENT_TOKEN"
)

// MountRequest asks for Source to be mounted at Target, both relative to the
// workspace.
type MountRequest struct {
	Source string `json:"source"`
	Target string `json:"target"`
	mount.Opts
}

// TargetRequest names a mount target, relative to the workspace.
type TargetRequest struct {
	Target string `json:"target"`
}

// MountedResponse answers /v1/mounted.
type MountedResponse struct {
	Mounted bool `json:"mounted"`
}

// ErrorResponse carries a failure's message.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Client is the sidecar's Mounter when the node agent performs the mounts.
// Paths are relative to the workspace, as for every Mounter.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewClient returns a client for the agent at endpoint (http://<node>:<port>).
func NewClient(endpoint, token string) *Client {
	return &Client{endpoint: strings.TrimSuffix(endpoint, "/"), token: token, http: &http.Client{Timeout: 2 * time.Minute}}
}

// Mount implements the sidecar's Mounter.
func (c *Client) Mount(source, target string, opts mount.Opts) error {
	return c.call("/v1/mount", MountRequest{Source: source, Target: target, Opts: opts}, nil)
}

// Unmount implements the sidecar's Mounter.
func (c *Client) Unmount(target string) error {
	return c.call("/v1/unmount", TargetRequest{Target: target}, nil)
}

// IsMounted implements the sidecar's Mounter.
func (c *Client) IsMounted(target string) (bool, error) {
	var resp MountedResponse
	err := c.call("/v1/mounted", TargetRequest{Target: target}, &resp)
	return resp.Mounted, err
}

// retryFor bounds how long a call waits for an agent that is not accepting
// connections — the window of a DaemonSet rolling update — before failing.
const retryFor = time.Minute

func (c *Client) call(path string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(retryFor)
	for wait := 250 * time.Millisecond; ; wait = min(2*wait, 5*time.Second) {
		err = c.post(path, body, resp)
		var dial *net.OpError
		if err == nil || !errors.As(err, &dial) || dial.Op != "dial" || time.Now().Add(wait).After(deadline) {
			return err
		}
		time.Sleep(wait)
	}
}

func (c *Client) post(path string, body []byte, resp any) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mount agent: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e ErrorResponse
		if json.NewDecoder(res.Body).Decode(&e) == nil && e.Error != "" {
			return fmt.Errorf("mount agent: %s", e.Error)
		}
		return fmt.Errorf("mount agent: %s", res.Status)
	}
	if resp == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(resp)
}
