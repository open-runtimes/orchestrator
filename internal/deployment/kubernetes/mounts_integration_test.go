//go:build k8s_integration

package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/deployment"
	"orchestrator/internal/kube"
	"orchestrator/internal/mountagent"
	"orchestrator/internal/pool"
	"orchestrator/internal/testutil"
	"orchestrator/internal/warm"
	"orchestrator/internal/workload"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const poolShimImage = "ko.local/pool-shim:latest"

// mountRequest serves only once the squashfs image its artifacts build is
// mounted at /workspace/code, so reaching ready proves the mount reached the
// worker.
func mountRequest(id string) *deployment.Request {
	req := serverRequest(id)
	req.Command = `grep -q "mounted content" /workspace/code/hello.txt && exec /agnhost netexec --http-port=8080`
	req.Artifacts = artifact.Set{
		&artifact.Write{ID: "w", In: "mounted content", Out: "hello.txt"},
		&artifact.Archive{ID: "a", In: "hello.txt", Out: "code.sqfs", Format: "squashfs", Depends: "w"},
		&artifact.Mount{ID: "m", In: "code.sqfs", Out: "code", Depends: "a"},
	}
	return req
}

// mountModes runs a test with the mounting sidecar privileged in the pod, and
// with the node mount agent mounting instead (the agent DaemonSet must be
// installed; task test-k8s-integration does it).
var mountModes = []struct {
	name string
	port int
}{{"sidecar", 0}, {"agent", mountagent.DefaultPort}}

// TestIntegration_Mounts deploys a revision whose code is a mounted image.
// The agent run uses two replicas, which share one pod template, so it also
// proves each replica got its own workspace token: a shared token would bind
// the second pod's workspace to nothing and leave it unready.
func TestIntegration_Mounts(t *testing.T) {
	for _, mode := range mountModes {
		t.Run(mode.name, func(t *testing.T) {
			o, teardown := setup(t, func(c *Config) { c.MountAgentPort = mode.port })
			defer teardown()

			id := fmt.Sprintf("mnt-%s-%d", mode.name, time.Now().UnixNano()%1_000_000_000)
			req := mountRequest(id)
			req.Replicas = 2
			if _, err := o.Apply(t.Context(), req); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			waitForReplicas(t, o, id, 2)
			assertMountPods(t, o, id, mode.port > 0)
		})
	}
}

// TestIntegration_PooledMounts claims a warm pool pod for a revision whose
// code is a mounted image. The pod's worker is already running when the claim
// arrives, so this is the late mount: it reaches the worker only by
// propagation into a running container.
func TestIntegration_PooledMounts(t *testing.T) {
	for _, mode := range mountModes {
		t.Run(mode.name, func(t *testing.T) {
			id := fmt.Sprintf("pmnt-%s-%d", mode.name, time.Now().UnixNano()%1_000_000_000)
			req := mountRequest(id)
			p := pool.Pool{ID: "mounts", Size: 1, Burst: pool.BurstReject, Spec: pool.Spec{
				Image: req.Image, Port: req.Port, CPU: req.CPU, Memory: req.Memory, Mounts: true,
			}}
			o, teardown := setup(t, func(c *Config) {
				c.MountAgentPort = mode.port
				c.Pools = []pool.Pool{p}
				c.PoolShimImage = poolShimImage
			})
			defer teardown()

			testutil.MustWaitFor(t, func() bool {
				pods, err := o.pools.Pods(t.Context(), p.ID)
				return err == nil && len(pods) == 1 && o.pools.Claimable(&pods[0])
			}, testutil.WithTimeout(120*time.Second), testutil.WithInterval(time.Second))

			if _, err := o.Apply(t.Context(), req); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			waitForReplicas(t, o, id, 1)
			for _, pod := range assertMountPods(t, o, id, mode.port > 0) {
				if pod.Labels[LabelPoolClaim] == "" {
					t.Errorf("pod %s was created cold, want the claimed warm pod", pod.Name)
				}
			}
		})
	}
}

func waitForReplicas(t *testing.T, o *Orchestrator, id string, n int) {
	t.Helper()
	testutil.MustWaitFor(t, func() bool {
		s, err := o.Status(t.Context(), id)
		return err == nil && s.State == deployment.StateReady && s.AvailableReplicas == n
	}, testutil.WithTimeout(150*time.Second), testutil.WithInterval(time.Second))
}

// assertMountPods lists the deployment's pods; in agent mode none may have run
// a privileged container.
func assertMountPods(t *testing.T, o *Orchestrator, id string, agent bool) []corev1.Pod {
	t.Helper()
	pods, err := o.client.CoreV1().Pods(testNamespace).List(t.Context(), metav1.ListOptions{LabelSelector: LabelDeploymentID + "=" + id})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("list pods: %v (found %d)", err, len(pods.Items))
	}
	for _, pod := range pods.Items {
		for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			if sc := c.SecurityContext; agent && sc != nil && sc.Privileged != nil && *sc.Privileged {
				t.Errorf("pod %s container %s ran privileged in agent mode", pod.Name, c.Name)
			}
		}
	}
	return pods.Items
}

// startTunnelledPools swaps the orchestrator's pool manager for one whose
// sidecar calls go through port-forwards — pod IPs are not routable from the
// host on kind — and runs the pool controller's inventory loop over the same
// configuration until the returned stop is called.
func startTunnelledPools(t *testing.T, o *Orchestrator) (*warm.Manager, func()) {
	t.Helper()
	restCfg, err := kube.NewConfig(o.cfg.Kubeconfig, o.cfg.Context, nil, 50, 100)
	if err != nil {
		t.Fatalf("rest config: %v", err)
	}
	wc := revisionWarmConfig(o.cfg)
	wc.Client = &tunnelClient{restCfg: restCfg, o: o}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		warm.New(o.client, o.cfg.Pools, wc).RunControl(ctx, warm.Hooks{})
	}()
	return warm.New(o.client, o.cfg.Pools, wc), func() { cancel(); <-done }
}

// tunnelClient is the warm pool's sidecar client for a test on the host: each
// call reaches the pod's admin port through a port-forward.
type tunnelClient struct {
	restCfg *rest.Config
	o       *Orchestrator
}

func (c *tunnelClient) Claim(ctx context.Context, podIP, token string, req *workload.ClaimRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	status, msg, err := c.call(ctx, podIP, http.MethodPost, workload.ClaimPath, token, body)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("claim rejected with status %d: %s", status, msg)
	}
	return err
}

func (c *tunnelClient) State(ctx context.Context, podIP string) (*workload.ClaimState, error) {
	var state workload.ClaimState
	return &state, c.getJSON(ctx, podIP, workload.ClaimStatePath, &state)
}

func (c *tunnelClient) Ready(ctx context.Context, podIP string) bool {
	status, _, err := c.call(ctx, podIP, http.MethodGet, "/ready", "", nil)
	return err == nil && status == http.StatusOK
}

func (c *tunnelClient) Requests(ctx context.Context, podIP string) (int64, error) {
	var stats struct {
		Requests int64 `json:"requests"`
	}
	return stats.Requests, c.getJSON(ctx, podIP, "/stats", &stats)
}

func (c *tunnelClient) getJSON(ctx context.Context, podIP, path string, out any) error {
	status, body, err := c.call(ctx, podIP, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, status)
	}
	return json.Unmarshal(body, out)
}

// call forwards a local port to the admin port of the pod holding podIP for
// the length of one request.
func (c *tunnelClient) call(ctx context.Context, podIP, method, path, token string, body []byte) (int, []byte, error) {
	pods, err := c.o.client.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{FieldSelector: "status.podIP=" + podIP})
	if err != nil {
		return 0, nil, err
	}
	if len(pods.Items) == 0 {
		return 0, nil, errors.New("no pod holds " + podIP)
	}
	url := c.o.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(testNamespace).
		Name(pods.Items[0].Name).SubResource("portforward").URL()
	transport, upgrader, err := spdy.RoundTripperFor(c.restCfg)
	if err != nil {
		return 0, nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"},
		[]string{"0:" + strconv.Itoa(workload.DefaultAdminPort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return 0, nil, err
	}
	failed := make(chan error, 1)
	go func() { failed <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-failed:
		return 0, nil, fmt.Errorf("port-forward: %w", err)
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method,
		"http://127.0.0.1:"+strconv.Itoa(int(ports[0].Local))+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	msg, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, bytes.TrimSpace(msg), err
}
