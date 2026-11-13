package kube

import (
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

func TestNewConfig_ExplicitBudgetPassesThrough(t *testing.T) {
	config := newTestConfig(t, 50, 60)
	if qps := config.RateLimiter.QPS(); qps != 50 {
		t.Errorf("NewConfig(qps=50, burst=60) rate limiter QPS = %v, want 50", qps)
	}
	if burst := drain(config.RateLimiter); burst < 60 || burst > 60+50 {
		t.Errorf("NewConfig(qps=50, burst=60) accepted %d requests in a burst, want 60", burst)
	}
}

func TestNewConfig_ZeroBurstTakesTheDefaultBurst(t *testing.T) {
	config := newTestConfig(t, 1, 0)
	if burst := drain(config.RateLimiter); burst != DefaultClientBurst {
		t.Errorf("NewConfig(qps=1, burst=0) accepted %d requests in a burst, want DefaultClientBurst (%d)", burst, DefaultClientBurst)
	}
}

func TestNewConfig_ZeroBudgetTakesTheDefaultBurst(t *testing.T) {
	config := newTestConfig(t, 0, 0)
	if burst := drain(config.RateLimiter); burst < DefaultClientBurst || burst > DefaultClientBurst+DefaultClientQPS {
		t.Errorf("NewConfig(qps=0, burst=0) accepted %d requests in a burst, want DefaultClientBurst (%d)", burst, DefaultClientBurst)
	}
}

func newTestConfig(t *testing.T, qps float32, burst int) *rest.Config {
	t.Helper()
	config, err := NewConfig(writeKubeconfig(t), "", nil, qps, burst)
	if err != nil {
		t.Fatalf("NewConfig: %v", err)
	}
	if config.RateLimiter == nil {
		t.Fatalf("NewConfig(qps=%v, burst=%d) installed no rate limiter", qps, burst)
	}
	return config
}

func drain(limiter flowcontrol.RateLimiter) int {
	accepted := 0
	for limiter.TryAccept() {
		accepted++
	}
	return accepted
}
