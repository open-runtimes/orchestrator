package pool

import (
	"strings"
	"testing"
)

func TestLoadPools_Sandbox(t *testing.T) {
	t.Parallel()

	pools, err := LoadPools(`[
		{"id":"a","image":"node:20","port":3000},
		{"id":"b","image":"node:20","port":3000,"sandbox":"runc"},
		{"id":"c","image":"node:20","port":3000,"sandbox":"gvisor"},
		{"id":"d","image":"node:20","port":3000,"sandbox":"kata"}
	]`)
	if err != nil {
		t.Fatalf("valid sandboxes: %v", err)
	}
	if len(pools) != 4 {
		t.Fatalf("want 4 pools, got %d", len(pools))
	}

	_, err = LoadPools(`[{"id":"a","image":"node:20","port":3000,"sandbox":"firecracker"}]`)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("invalid sandbox: want sandbox error, got %v", err)
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
	if pools[0].Burst != BurstCold {
		t.Errorf("default burst = %q, want cold", pools[0].Burst)
	}
	if pools[1].Burst != BurstReject {
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
