package docker

import (
	"encoding/json"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"slices"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

func TestDeriveStatus(t *testing.T) {
	t.Parallel()

	now := time.Now()

	tests := []struct {
		name          string
		snap          snapshot
		wantState     string
		wantAvailable int
		wantError     bool
	}{
		{
			name: "worker and healthy proxy running is ready",
			snap: snapshot{
				workerExists: true, workerRunning: true,
				proxyRunning: true, proxyHealth: container.Healthy,
				created: now.Add(-time.Minute), deadline: 10 * time.Minute,
			},
			wantState:     deployment.StateReady,
			wantAvailable: 1,
		},
		{
			name: "worker exited is failed with exit code",
			snap: snapshot{
				workerExists: true, workerRunning: false, workerExitCode: 137,
				proxyRunning: true, proxyHealth: container.Starting,
				created: now.Add(-time.Second), deadline: 10 * time.Minute,
			},
			wantState: deployment.StateFailed,
			wantError: true,
		},
		{
			name: "starting health within deadline is pending",
			snap: snapshot{
				workerExists: true, workerRunning: true,
				proxyRunning: true, proxyHealth: container.Starting,
				created: now.Add(-time.Second), deadline: 10 * time.Minute,
			},
			wantState: deployment.StatePending,
		},
		{
			name: "unhealthy past deadline is failed",
			snap: snapshot{
				workerExists: true, workerRunning: true,
				proxyRunning: true, proxyHealth: container.Unhealthy,
				created: now.Add(-time.Hour), deadline: 10 * time.Minute,
			},
			wantState: deployment.StateFailed,
			wantError: true,
		},
		{
			name: "proxy missing within deadline is pending",
			snap: snapshot{
				workerExists: true, workerRunning: true,
				created: now.Add(-time.Second), deadline: 10 * time.Minute,
			},
			wantState: deployment.StatePending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := deriveStatus("dep1", tt.snap, now)

			if got.ID != "dep1" {
				t.Errorf("ID = %q, want dep1", got.ID)
			}
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
			if got.DesiredReplicas != 1 {
				t.Errorf("DesiredReplicas = %d, want 1", got.DesiredReplicas)
			}
			if got.AvailableReplicas != tt.wantAvailable {
				t.Errorf("AvailableReplicas = %d, want %d", got.AvailableReplicas, tt.wantAvailable)
			}
			if (got.Error != "") != tt.wantError {
				t.Errorf("Error = %q, wantError %v", got.Error, tt.wantError)
			}
		})
	}
}

func TestDeriveStatus_WorkerExitCodeInError(t *testing.T) {
	t.Parallel()

	snap := snapshot{workerExists: true, workerExitCode: 2, deadline: time.Minute}
	got := deriveStatus("dep1", snap, time.Now())

	if got.Error != "worker exited with code 2" {
		t.Errorf("Error = %q, want worker exit code message", got.Error)
	}
}

func TestContainerIP(t *testing.T) {
	t.Parallel()

	settings := &container.NetworkSettings{
		Networks: map[string]*network.EndpointSettings{
			"bridge":       {IPAddress: "172.17.0.2"},
			"orchestrator": {IPAddress: "10.5.0.7"},
		},
	}

	tests := []struct {
		name        string
		settings    *container.NetworkSettings
		networkName string
		want        string
	}{
		{"nil settings", nil, "", ""},
		{"named network", settings, "orchestrator", "10.5.0.7"},
		{"default bridge", settings, "", "172.17.0.2"},
		{"unattached network", settings, "other", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containerIP(tt.settings, tt.networkName); got != tt.want {
				t.Errorf("containerIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProxyEnv_Minimal(t *testing.T) {
	t.Parallel()

	req := &deployment.Request{ID: "d1", Port: 3000}
	env := proxyEnv(req, "10.0.0.5")

	want := []string{proxy.EnvTarget + "=10.0.0.5:3000"}
	if !slices.Equal(env, want) {
		t.Errorf("proxyEnv() = %v, want %v", env, want)
	}
}

func TestProxyEnv_Full(t *testing.T) {
	t.Parallel()

	req := &deployment.Request{
		ID:             "d1",
		Port:           8080,
		TimeoutSeconds: 120,
		Concurrency:    10,
		Probes: &deployment.Probes{
			Readiness: &deployment.Probe{
				Path:             "/healthz",
				PeriodMillis:     250,
				TimeoutMillis:    500,
				FailureThreshold: 5,
			},
		},
	}
	env := proxyEnv(req, "10.0.0.5")

	want := []string{
		proxy.EnvTarget + "=10.0.0.5:8080",
		proxy.EnvTimeoutSeconds + "=120",
		proxy.EnvConcurrency + "=10",
		proxy.EnvReadinessPath + "=/healthz",
		proxy.EnvReadinessPeriodMillis + "=250",
		proxy.EnvReadinessTimeoutMillis + "=500",
		proxy.EnvReadinessFailureThreshold + "=5",
	}
	if !slices.Equal(env, want) {
		t.Errorf("proxyEnv() = %v, want %v", env, want)
	}
}

func TestProgressDeadline(t *testing.T) {
	t.Parallel()

	if got := progressDeadline(0); got != defaultProgressDeadline {
		t.Errorf("progressDeadline(0) = %v, want %v", got, defaultProgressDeadline)
	}
	if got := progressDeadline(5); got != 5*time.Second {
		t.Errorf("progressDeadline(5) = %v, want 5s", got)
	}
}

func TestSpecOf(t *testing.T) {
	t.Parallel()

	summaries := []container.Summary{
		{Labels: map[string]string{labelType: typeWorker}},
		{Labels: map[string]string{labelType: typeProxy, labelSpec: `{"id":"d1"}`}},
	}
	if got := specOf(summaries); got != `{"id":"d1"}` {
		t.Errorf("specOf() = %q, want spec JSON", got)
	}
	if got := specOf(summaries[:1]); got != "" {
		t.Errorf("specOf() without proxy = %q, want empty", got)
	}
}

func TestSpecDeadline(t *testing.T) {
	t.Parallel()

	spec, err := json.Marshal(&deployment.Request{ID: "d1", ProgressDeadlineSeconds: 30})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	summaries := []container.Summary{
		{Labels: map[string]string{labelType: typeProxy, labelSpec: string(spec)}},
	}

	if got := specDeadline(summaries); got != 30*time.Second {
		t.Errorf("specDeadline() = %v, want 30s", got)
	}
	if got := specDeadline(nil); got != defaultProgressDeadline {
		t.Errorf("specDeadline(nil) = %v, want default", got)
	}
}

func TestContainerNames(t *testing.T) {
	t.Parallel()

	if got := workerName("d1"); got != "dep-d1-worker" {
		t.Errorf("workerName = %q", got)
	}
	if got := proxyName("d1"); got != "dep-d1-proxy" {
		t.Errorf("proxyName = %q", got)
	}
	if got := artifactsName("d1"); got != "dep-d1-artifacts" {
		t.Errorf("artifactsName = %q", got)
	}
	if got := volumeName("d1"); got != "dep-d1-workspace" {
		t.Errorf("volumeName = %q", got)
	}
}
