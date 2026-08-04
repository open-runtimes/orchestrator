package kubernetes

import (
	"context"
	"orchestrator/pkg/deployment"
	"orchestrator/pkg/pool"
	"strings"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildWarmPod_RuntimeClass(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.RuntimeClasses[deployment.RuntimeClassKata] = "kata-qemu"

	for tier, want := range map[string]string{
		"":                            "",
		deployment.RuntimeClassRunc:   "",
		deployment.RuntimeClassGvisor: "gvisor",
		deployment.RuntimeClassKata:   "kata-qemu",
	} {
		p := mapperPool()
		p.RuntimeClass = tier
		got := buildWarmPod(p, cfg, "pool-std-1", "token").Spec.RuntimeClassName
		switch {
		case want == "" && got != nil:
			t.Errorf("tier %q: want no runtimeClassName, got %q", tier, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("tier %q: want runtimeClassName %q, got %v", tier, want, got)
		}
	}
}

func TestLoadConfigFromEnv_RuntimeClasses(t *testing.T) {
	t.Setenv("KUBE_RUNTIME_CLASSES", "gvisor=runsc")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.RuntimeClasses["gvisor"] != "runsc" || cfg.RuntimeClasses["kata"] != "kata" {
		t.Errorf("RuntimeClasses: got %v", cfg.RuntimeClasses)
	}

	t.Setenv("KUBE_RUNTIME_CLASSES", "runc=runc")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Error("runc mapping: want error")
	}
}

func TestStart_MissingRuntimeClassFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, cs, _ := newTestOrchestrator(t, pool.Pool{ID: "sbx", Image: "runtime:latest", Size: 1, RuntimeClass: deployment.RuntimeClassKata})

	// Operator config names an uninstalled RuntimeClass → Start fails loudly.
	err := o.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), `RuntimeClass "kata"`) {
		t.Fatalf("want missing-RuntimeClass error, got %v", err)
	}

	// Installed → the check passes.
	if _, err := cs.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "kata"},
		Handler:    "kata",
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	if err := o.checkRuntimeClasses(ctx); err != nil {
		t.Errorf("want ok, got %v", err)
	}
}
