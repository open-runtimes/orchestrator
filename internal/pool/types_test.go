package pool

import (
	"orchestrator/internal/claim"
	"strings"
	"testing"
)

func TestLoadPools_Volumes(t *testing.T) {
	t.Parallel()

	pools, err := LoadPools(`[{"id":"a","image":"node:20","port":3000,"volumes":[{"source":"cache-pvc","path":"/cache"}]}]`)
	if err != nil {
		t.Fatalf("valid volume rejected: %v", err)
	}
	if len(pools[0].Volumes) != 1 || pools[0].Volumes[0].Source != "cache-pvc" {
		t.Errorf("volumes not parsed: %+v", pools[0].Volumes)
	}

	if _, err := LoadPools(`[{"id":"a","image":"node:20","port":3000,"volumes":[{"source":"x","path":"relative"}]}]`); err == nil {
		t.Error("expected rejection of a relative volume path")
	}
}

func TestLoadPools_RuntimeClass(t *testing.T) {
	t.Parallel()

	pools, err := LoadPools(`[
		{"id":"a","image":"node:20","port":3000},
		{"id":"b","image":"node:20","port":3000,"runtimeClass":"runc"},
		{"id":"c","image":"node:20","port":3000,"runtimeClass":"gvisor"},
		{"id":"d","image":"node:20","port":3000,"runtimeClass":"kata"}
	]`)
	if err != nil {
		t.Fatalf("valid tiers: %v", err)
	}
	if len(pools) != 4 {
		t.Fatalf("want 4 pools, got %d", len(pools))
	}

	_, err = LoadPools(`[{"id":"a","image":"node:20","port":3000,"runtimeClass":"firecracker"}]`)
	if err == nil || !strings.Contains(err.Error(), "runtimeClass") {
		t.Errorf("invalid tier: want runtimeClass error, got %v", err)
	}
}

// An unset burst policy defaults to cold: an activation at an empty pool
// pays the cold start rather than failing with 429.
func TestLoadPools_BurstDefaultsToCold(t *testing.T) {
	t.Parallel()

	pools, err := LoadPools(`[
		{"id":"a","image":"node:20","port":3000},
		{"id":"b","image":"node:20","port":3000,"burst":"reject"}
	]`)
	if err != nil {
		t.Fatal(err)
	}
	if pools[0].Burst != claim.BurstCold {
		t.Errorf("default burst = %q, want cold", pools[0].Burst)
	}
	if pools[1].Burst != claim.BurstReject {
		t.Errorf("explicit burst = %q, want reject preserved", pools[1].Burst)
	}
}

func TestLoadPools_PortRequired(t *testing.T) {
	t.Parallel()
	_, err := LoadPools(`[{"id":"a","image":"node:20"}]`)
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("want port-required error, got %v", err)
	}
}
