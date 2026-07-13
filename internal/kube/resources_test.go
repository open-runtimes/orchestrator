package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestWorkerResources_CPUOvercommit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cpu        float64
		overcommit float64
		wantMilli  int64
	}{
		"no overcommit":         {cpu: 0.5, overcommit: 1, wantMilli: 500},
		"overcommit 4":          {cpu: 0.5, overcommit: 4, wantMilli: 125},
		"zero means 1":          {cpu: 0.5, overcommit: 0, wantMilli: 500},
		"negative means 1":      {cpu: 2, overcommit: -3, wantMilli: 2000},
		"rounds up":             {cpu: 1, overcommit: 3, wantMilli: 334},
		"floored at 1m":         {cpu: 0.001, overcommit: 8, wantMilli: 1},
		"multi-core overcommit": {cpu: 8, overcommit: 4, wantMilli: 2000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := Overcommit{CPU: tc.overcommit}.WorkerResources(tc.cpu, 128)
			if got := r.Requests.Cpu().MilliValue(); got != tc.wantMilli {
				t.Errorf("cpu request: want %dm, got %dm", tc.wantMilli, got)
			}
			if _, ok := r.Limits[corev1.ResourceCPU]; ok {
				t.Errorf("cpu limit: want none, got %s", r.Limits.Cpu())
			}
			if !r.Requests.Memory().Equal(*r.Limits.Memory()) || r.Limits.Memory().Value() != 128*1024*1024 {
				t.Errorf("memory: want request == limit == 128Mi, got %+v", r)
			}
		})
	}
}

func TestWorkerResources_MemoryOvercommit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		memoryMi   int
		overcommit float64
		wantMi     int64
	}{
		"no overcommit":    {memoryMi: 128, overcommit: 1, wantMi: 128},
		"overcommit 2":     {memoryMi: 128, overcommit: 2, wantMi: 64},
		"zero means 1":     {memoryMi: 128, overcommit: 0, wantMi: 128},
		"negative means 1": {memoryMi: 128, overcommit: -2, wantMi: 128},
		"rounds up":        {memoryMi: 100, overcommit: 3, wantMi: 34},
		"floored at 1Mi":   {memoryMi: 1, overcommit: 8, wantMi: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := Overcommit{Memory: tc.overcommit}.WorkerResources(0, tc.memoryMi)
			if got := r.Requests.Memory().Value(); got != tc.wantMi*1024*1024 {
				t.Errorf("memory request: want %dMi, got %s", tc.wantMi, r.Requests.Memory())
			}
			// The declared ceiling is always the limit, whatever the request.
			if got := r.Limits.Memory().Value(); got != int64(tc.memoryMi)*1024*1024 {
				t.Errorf("memory limit: want %dMi, got %s", tc.memoryMi, r.Limits.Memory())
			}
		})
	}
}

// A bare spec keeps no resources at all — nothing to derive.
func TestWorkerResources_ZeroSpec(t *testing.T) {
	t.Parallel()
	r := Overcommit{CPU: 4, Memory: 2}.WorkerResources(0, 0)
	if len(r.Limits) != 0 || len(r.Requests) != 0 {
		t.Errorf("expected empty resources, got %+v", r)
	}
}

func TestTolerationsFromEnv(t *testing.T) {
	t.Setenv("KUBE_WORKLOAD_TOLERATIONS", `[{"key":"workload","value":"edge-builds","effect":"NoSchedule"}]`)
	ts, err := TolerationsFromEnv()
	if err != nil || len(ts) != 1 {
		t.Fatalf("want 1 toleration, got %v (err %v)", ts, err)
	}
	if ts[0].Key != "workload" || ts[0].Value != "edge-builds" || ts[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("toleration round-trip: got %+v", ts[0])
	}

	t.Setenv("KUBE_WORKLOAD_TOLERATIONS", "")
	if ts, err := TolerationsFromEnv(); err != nil || ts != nil {
		t.Errorf("empty env: want nil, got %v (err %v)", ts, err)
	}

	// Malformed JSON must fail loudly, not silently strand pods.
	t.Setenv("KUBE_WORKLOAD_TOLERATIONS", "{not json")
	if _, err := TolerationsFromEnv(); err == nil {
		t.Error("malformed JSON: want error")
	}
}

func TestOvercommitFromEnv(t *testing.T) {
	t.Setenv("KUBE_CPU_OVERCOMMIT", "4")
	t.Setenv("KUBE_MEMORY_OVERCOMMIT", "1.5")
	if o := OvercommitFromEnv(); o.CPU != 4 || o.Memory != 1.5 {
		t.Errorf("want {4 1.5}, got %+v", o)
	}
}
