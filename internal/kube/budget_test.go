package kube

import "testing"

const serviceQPS = 200

func TestNewConfig_ZeroBudgetIsTheServiceBudget(t *testing.T) {
	config, err := NewConfig(writeKubeconfig(t), "", nil, 0, 0)
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if config.RateLimiter == nil {
		t.Fatal("NewConfig(qps=0, burst=0) installed no shared rate limiter, so each API group falls back to client-go's 5 QPS / burst 10")
	}
	if qps := config.RateLimiter.QPS(); qps != serviceQPS {
		t.Errorf("NewConfig(qps=0, burst=0) rate limiter QPS = %v, want the service budget of %v", qps, serviceQPS)
	}
}
