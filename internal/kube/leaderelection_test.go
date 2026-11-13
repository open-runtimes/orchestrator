package kube

import "testing"

func TestNewLeaseClient_HasItsOwnBudget(t *testing.T) {
	config := newTestConfig(t, 1, 1)
	drain(config.RateLimiter)

	client, err := NewLeaseClient(config)
	if err != nil {
		t.Fatalf("NewLeaseClient: %v", err)
	}
	limiter := client.RESTClient().GetRateLimiter()
	if limiter == nil {
		t.Fatal("lease client has no rate limiter, so client-go would give it a default one per client")
	}
	if qps := limiter.QPS(); qps != leaseQPS {
		t.Errorf("lease client QPS = %v, want %v", qps, leaseQPS)
	}
	if !limiter.TryAccept() {
		t.Error("lease client refused a renewal while only the shared budget was drained")
	}
	if config.RateLimiter.TryAccept() {
		t.Error("shared budget accepted a request after draining, want the lease client to leave it untouched")
	}
}
