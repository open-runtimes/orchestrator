package docker

import (
	"orchestrator/pkg/pool"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

func TestSlotNaming(t *testing.T) {
	cases := []struct {
		fn   func(string, string) string
		want string
	}{
		{volumeName, "pool-py-a1b2-ws"},
		{sidecarName, "pool-py-a1b2-sidecar"},
		{workloadName, "pool-py-a1b2-workload"},
		{installName, "pool-py-a1b2-shim"},
	}
	for _, c := range cases {
		if got := c.fn("py", "a1b2"); got != c.want {
			t.Errorf("name = %q, want %q", got, c.want)
		}
	}
}

func TestSlotLabels(t *testing.T) {
	labels := slotLabels("py", "a1b2", typeSidecar)
	want := map[string]string{
		labelManagedBy: "deployments-service",
		labelPoolID:    "py",
		labelSlot:      "a1b2",
		labelType:      "sidecar",
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("labels[%s] = %q, want %q", k, labels[k], v)
		}
	}

	// Volume labels carry no type.
	if _, ok := slotLabels("py", "a1b2", "")[labelType]; ok {
		t.Error("expected no type label on volume labels")
	}
}

func TestRandomHex(t *testing.T) {
	a, b := randomHex(4), randomHex(4)
	if len(a) != 8 || len(b) != 8 {
		t.Errorf("randomHex(4) lengths = %d, %d, want 8", len(a), len(b))
	}
	if a == b {
		t.Error("expected distinct random values")
	}
}

func TestActivationState(t *testing.T) {
	cases := []struct {
		exists, running bool
		want            string
	}{
		{false, false, pool.StateFailed},
		{true, true, pool.StateReady},
		{true, false, pool.StateExited},
	}
	for _, c := range cases {
		if got := activationState(c.exists, c.running); got != c.want {
			t.Errorf("activationState(%v, %v) = %q, want %q", c.exists, c.running, got, c.want)
		}
	}
}

func TestContainerIP(t *testing.T) {
	networks := map[string]*network.EndpointSettings{
		"bridge": {IPAddress: "172.17.0.2"},
		"custom": {IPAddress: "10.0.0.2"},
	}
	if got := containerIP(networks, ""); got != "172.17.0.2" {
		t.Errorf("default network IP = %q, want 172.17.0.2", got)
	}
	if got := containerIP(networks, "custom"); got != "10.0.0.2" {
		t.Errorf("custom network IP = %q, want 10.0.0.2", got)
	}
	if got := containerIP(networks, "missing"); got != "" {
		t.Errorf("missing network IP = %q, want empty", got)
	}
}

func TestCappedWriter(t *testing.T) {
	w := &cappedWriter{cap: 10}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if got := w.String(); got != "hello" {
		t.Errorf("under-cap output = %q, want hello", got)
	}

	if _, err := w.Write([]byte("world!!!")); err != nil {
		t.Fatal(err)
	}
	got := w.String()
	if !strings.HasPrefix(got, "helloworld") || !strings.Contains(got, "truncated") {
		t.Errorf("over-cap output = %q, want first 10 bytes + truncation flag", got)
	}
}

func TestGroupSlots(t *testing.T) {
	summaries := []container.Summary{
		{Labels: map[string]string{labelPoolID: "py", labelSlot: "s1", labelType: typeSidecar}},
		{Labels: map[string]string{labelPoolID: "py", labelSlot: "s1", labelType: typeWorkload}},
		{Labels: map[string]string{labelPoolID: "py", labelSlot: "s2", labelType: typeWorkload}},
		{Labels: map[string]string{labelPoolID: "go", labelSlot: "s3", labelType: typeSidecar}},
		{Labels: map[string]string{"unrelated": "x"}},
	}
	byPool := groupSlots(summaries)

	if len(byPool) != 2 || len(byPool["py"]) != 2 || len(byPool["go"]) != 1 {
		t.Fatalf("groupSlots = %v pools (py=%d, go=%d), want 2 (py=2, go=1)",
			len(byPool), len(byPool["py"]), len(byPool["go"]))
	}
	if byPool["py"]["s1"].sidecar == nil || byPool["py"]["s1"].workload == nil {
		t.Error("slot s1 should have both sidecar and workload")
	}
	if byPool["py"]["s2"].sidecar != nil {
		t.Error("slot s2 should have no sidecar")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("DOCKER_NETWORK", "testnet")
	t.Setenv("EXTRA_HOSTS", "a.test:host-gateway,b.test:host-gateway")
	t.Setenv("POOL_ACTIVATION_RETENTION", "1m")

	cfg := LoadConfigFromEnv()
	if cfg.Network != "testnet" {
		t.Errorf("Network = %q, want testnet", cfg.Network)
	}
	if len(cfg.ExtraHosts) != 2 || cfg.ExtraHosts[0] != "a.test:host-gateway" {
		t.Errorf("ExtraHosts = %v, want two entries", cfg.ExtraHosts)
	}
	if cfg.Retention != time.Minute {
		t.Errorf("Retention = %s, want 1m", cfg.Retention)
	}
}

func TestLoadConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("DOCKER_NETWORK", "")
	t.Setenv("EXTRA_HOSTS", "")
	t.Setenv("POOL_ACTIVATION_RETENTION", "")

	cfg := LoadConfigFromEnv()
	if cfg.Retention != 15*time.Minute {
		t.Errorf("Retention = %s, want 15m default", cfg.Retention)
	}
	if cfg.ExtraHosts != nil {
		t.Errorf("ExtraHosts = %v, want nil", cfg.ExtraHosts)
	}
}
