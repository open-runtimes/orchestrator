package kubernetes

import (
	"testing"
	"time"
)

func TestApplyDefaults_CPUOvercommit(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]float64{0: 1, -2: 1, 1: 1, 4: 4} {
		c := Config{CPUOvercommit: in}
		c.applyDefaults()
		if c.CPUOvercommit != want {
			t.Errorf("CPUOvercommit(%v): want %v, got %v", in, want, c.CPUOvercommit)
		}
	}
}

func TestApplyDefaults_LeaderElection(t *testing.T) {
	t.Parallel()

	// Disabled: left untouched (single-replica mode needs no lease identity).
	c := Config{}
	c.applyDefaults()
	if c.LeaderElection.Enabled || c.LeaderElection.LeaseName != "" {
		t.Errorf("disabled election must stay zero, got %+v", c.LeaderElection)
	}

	// Enabled: lease name, identity, and timings are defaulted.
	c = Config{}
	c.LeaderElection.Enabled = true
	c.applyDefaults()
	le := c.LeaderElection
	if le.LeaseName != "deployments-service-leader" {
		t.Errorf("LeaseName: want deployments-service-leader, got %s", le.LeaseName)
	}
	if le.Identity == "" {
		t.Error("Identity: want a defaulted identity")
	}
	if le.LeaseDuration != 15*time.Second || le.RenewDeadline != 10*time.Second || le.RetryPeriod != 2*time.Second {
		t.Errorf("timings: want 15s/10s/2s, got %v/%v/%v", le.LeaseDuration, le.RenewDeadline, le.RetryPeriod)
	}
}

func TestLoadConfigFromEnv_LeaderElection(t *testing.T) {
	t.Setenv("KUBE_LEADER_ELECTION", "true")
	t.Setenv("KUBE_LEADER_LEASE_NAME", "custom-lease")
	t.Setenv("KUBE_LEADER_IDENTITY", "pod-0")
	t.Setenv("KUBE_LEADER_LEASE_DURATION", "30s")
	t.Setenv("KUBE_CPU_OVERCOMMIT", "4")

	cfg := LoadConfigFromEnv()
	le := cfg.LeaderElection
	if !le.Enabled || le.LeaseName != "custom-lease" || le.Identity != "pod-0" || le.LeaseDuration != 30*time.Second {
		t.Errorf("LeaderElection from env: got %+v", le)
	}
	if le.RenewDeadline != 10*time.Second || le.RetryPeriod != 2*time.Second {
		t.Errorf("timing defaults: got %v/%v", le.RenewDeadline, le.RetryPeriod)
	}
	if cfg.CPUOvercommit != 4 {
		t.Errorf("CPUOvercommit: want 4, got %v", cfg.CPUOvercommit)
	}
}
