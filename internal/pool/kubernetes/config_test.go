package kubernetes

import (
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	t.Parallel()
	c := Config{}
	c.applyDefaults()

	if c.Namespace != "orchestrator" || c.RunAsUser != 65532 {
		t.Errorf("namespace/uid defaults: got %s/%d", c.Namespace, c.RunAsUser)
	}
	if c.GatewayName != "orchestrator" || c.GatewayNamespace != "orchestrator" {
		t.Errorf("gateway defaults: got %s/%s", c.GatewayName, c.GatewayNamespace)
	}
	if c.PoolDomain != "localhost" {
		t.Errorf("PoolDomain: want localhost, got %s", c.PoolDomain)
	}
	if c.OrphanTTL != 60*time.Second {
		t.Errorf("OrphanTTL default: got %v", c.OrphanTTL)
	}

	// Enabled election defaults the pools-specific lease name.
	c = Config{}
	c.LeaderElection.Enabled = true
	c.applyDefaults()
	if c.LeaderElection.LeaseName != "deployments-service-pools-leader" {
		t.Errorf("LeaseName: got %s", c.LeaderElection.LeaseName)
	}
	if c.LeaderElection.Identity == "" {
		t.Error("Identity: want a defaulted identity")
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("KUBE_NAMESPACE", "pools-ns")
	t.Setenv("KUBE_RUN_AS_USER", "1000")
	t.Setenv("KUBE_GATEWAY_ENABLED", "false")
	t.Setenv("KUBE_GATEWAY_NAME", "edge")
	t.Setenv("POOL_DOMAIN", "run.example.com")
	t.Setenv("POOL_ORPHAN_TTL", "90s")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.Namespace != "pools-ns" || cfg.RunAsUser != 1000 {
		t.Errorf("namespace/uid: got %s/%d", cfg.Namespace, cfg.RunAsUser)
	}
	if cfg.GatewayEnabled || cfg.GatewayName != "edge" {
		t.Errorf("gateway: got %v/%s", cfg.GatewayEnabled, cfg.GatewayName)
	}
	if cfg.PoolDomain != "run.example.com" {
		t.Errorf("PoolDomain: got %s", cfg.PoolDomain)
	}
	if cfg.OrphanTTL != 90*time.Second {
		t.Errorf("OrphanTTL: got %v", cfg.OrphanTTL)
	}
	if cfg.LeaderElection.LeaseName != "deployments-service-pools-leader" {
		t.Errorf("lease name: got %s", cfg.LeaderElection.LeaseName)
	}
}
