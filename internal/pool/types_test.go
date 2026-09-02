package pool

import (
	"encoding/json"
	"orchestrator/internal/claim"
	"orchestrator/internal/volume"
	"reflect"
	"strings"
	"testing"
)

func TestShapeKeyNormalizesEquivalentPodShapes(t *testing.T) {
	t.Parallel()
	a := Spec{Image: "img", Port: 3000, CPU: 1, Memory: 256,
		Volumes: []volume.Volume{{Source: "b", Path: "/b"}, {Source: "a", Path: "/a"}}}
	b := Spec{Image: "img", Port: 3000, CPU: 1, Memory: 256, RuntimeClass: "runc",
		TerminationGracePeriodSeconds: 30,
		Volumes: []volume.Volume{{Source: "a", Path: "/a"}, {Source: "b", Path: "/b"}}}
	if ShapeKey(&a) != ShapeKey(&b) {
		t.Fatal("equivalent runtime, grace, and volume ordering must match")
	}
	b.Mounts = true
	if ShapeKey(&a) == ShapeKey(&b) {
		t.Fatal("a pod-shaping capability must change the key")
	}
}

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

// An unset burst policy defaults to cold: a claim at an empty pool
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

// The pod shape lives in an embedded Spec, which Go flattens on the wire. That
// is the whole reason it is embedded rather than nested: POOLS_JSON is operator
// config rendered by the Helm chart, so the split must be invisible to it. A
// flat value must still parse, and marshalling one back must not grow a "spec"
// object.
func TestSpecIsFlatOnTheWire(t *testing.T) {
	raw := `[{"id":"web","image":"runtime:latest","port":8080,"size":2,"cpu":0.5,"memory":512,
		"runtimeClass":"gvisor","burst":"reject","mounts":true,"maxIdleSeconds":900,
		"terminationGracePeriodSeconds":120,"environment":{"K":"v"},
		"volumes":[{"source":"pvc","path":"/data"}]}]`
	pools, err := Load(raw, "POOLS_JSON")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	p := pools[0]
	if p.Image != "runtime:latest" || p.Port != 8080 || p.CPU != 0.5 || p.Memory != 512 ||
		p.RuntimeClass != "gvisor" || !p.Mounts || p.TerminationGracePeriodSeconds != 120 ||
		p.Environment["K"] != "v" || len(p.Volumes) != 1 {
		t.Errorf("flat shape fields did not land on the embedded Spec: %+v", p)
	}
	if p.Size != 2 || p.Burst != BurstReject || p.MaxIdleSeconds != 900 {
		t.Errorf("capacity policy did not land: %+v", p)
	}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), `"Spec"`) || strings.Contains(string(out), `"spec"`) {
		t.Errorf("the embedded Spec must not surface as a key, got %s", out)
	}
	var again Pool
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if !reflect.DeepEqual(again, p) {
		t.Errorf("round trip changed the pool:\n got %+v\nwant %+v", again, p)
	}
}
