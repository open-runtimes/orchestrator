package pool

import (
	"strings"
	"testing"
)

func TestLoadPools_Sandbox(t *testing.T) {
	t.Parallel()

	pools, err := LoadPools(`[
		{"id":"a","image":"node:20"},
		{"id":"b","image":"node:20","sandbox":"runc"},
		{"id":"c","image":"node:20","sandbox":"gvisor"},
		{"id":"d","image":"node:20","sandbox":"kata"}
	]`)
	if err != nil {
		t.Fatalf("valid sandboxes: %v", err)
	}
	if len(pools) != 4 {
		t.Fatalf("want 4 pools, got %d", len(pools))
	}

	_, err = LoadPools(`[{"id":"a","image":"node:20","sandbox":"firecracker"}]`)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Errorf("invalid sandbox: want sandbox error, got %v", err)
	}
}
