package mountagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"orchestrator/internal/mount"
	"path/filepath"
	"strings"
	"time"
)

// Environment the sidecar reads to use the agent instead of mounting itself.
// The mapper sets both; MOUNT_AGENT_ENDPOINT is built from the node's IP.
const (
	EnvEndpoint = "MOUNT_AGENT_ENDPOINT"
	EnvToken    = "MOUNT_AGENT_TOKEN"
)

// MountRequest asks for source to be mounted at target. Both are relative to
// the workspace; the agent resolves them beneath it.
type MountRequest struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Writable    bool   `json:"writable,omitempty"`
	SizeMiB     int    `json:"sizeMiB,omitempty"`
	SourceDir   bool   `json:"sourceDir,omitempty"`
	UpperOnDisk bool   `json:"upperOnDisk,omitempty"`
}

// TargetRequest names a mount target, relative to the workspace.
type TargetRequest struct {
	Target string `json:"target"`
}

// MountedResponse answers a TargetRequest to /v1/mounted.
type MountedResponse struct {
	Mounted bool `json:"mounted"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Client is the sidecar's Mounter when mounts are performed by the node agent.
// It speaks in the sidecar's absolute workspace paths, like the in-pod kernel
// mounter, and sends them to the agent relative to Workspace.
type Client struct {
	Endpoint  string // http://<node-ip>:<port>
	Token     string
	Workspace string // the workspace path in the sidecar's mount namespace
	HTTP      *http.Client
}

// NewClient builds a client with a timeout generous enough for a loop mount of
// a large image under node contention.
func NewClient(endpoint, token, workspace string) *Client {
	return &Client{Endpoint: strings.TrimSuffix(endpoint, "/"), Token: token, Workspace: workspace, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// Mount implements the sidecar's Mounter.
func (c *Client) Mount(source, target string, opts mount.Opts) error {
	src, err := c.rel(source)
	if err != nil {
		return err
	}
	dst, err := c.rel(target)
	if err != nil {
		return err
	}
	return c.call("/v1/mount", MountRequest{
		Source: src, Target: dst,
		Writable: opts.Writable, SizeMiB: opts.SizeMiB, SourceDir: opts.SourceDir, UpperOnDisk: opts.UpperOnDisk,
	}, nil)
}

// Unmount implements the sidecar's Mounter.
func (c *Client) Unmount(target string) error {
	dst, err := c.rel(target)
	if err != nil {
		return err
	}
	return c.call("/v1/unmount", TargetRequest{Target: dst}, nil)
}

// IsMounted implements the sidecar's Mounter.
func (c *Client) IsMounted(target string) (bool, error) {
	dst, err := c.rel(target)
	if err != nil {
		return false, err
	}
	var resp MountedResponse
	if err := c.call("/v1/mounted", TargetRequest{Target: dst}, &resp); err != nil {
		return false, err
	}
	return resp.Mounted, nil
}

// rel turns a sidecar path into one relative to the workspace, refusing
// anything outside it. The agent checks again; this only keeps a mistake in the
// runner from becoming a confusing remote error.
func (c *Client) rel(p string) (string, error) {
	rel, err := filepath.Rel(c.Workspace, p)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("mount path %s is not inside the workspace %s", p, c.Workspace)
	}
	return rel, nil
}

func (c *Client) call(path string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		return fmt.Errorf("mount agent: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var e errorResponse
		if json.NewDecoder(res.Body).Decode(&e) == nil && e.Error != "" {
			return fmt.Errorf("mount agent: %s", e.Error)
		}
		return fmt.Errorf("mount agent: %s", res.Status)
	}
	if resp == nil {
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(resp); err != nil {
		return errors.Join(errors.New("mount agent: undecodable response"), err)
	}
	return nil
}
