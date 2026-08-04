package kubernetes

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/pkg/deployment"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildDeployment_RuntimeClass(t *testing.T) {
	t.Parallel()
	cfg := Config{SidecarImage: "sidecar:latest"}
	cfg.applyDefaults()
	cfg.RuntimeClasses[deployment.RuntimeClassKata] = "kata-qemu"

	for tier, want := range map[string]string{
		"":                            "",
		deployment.RuntimeClassRunc:   "",
		deployment.RuntimeClassGvisor: "gvisor",
		deployment.RuntimeClassKata:   "kata-qemu",
	} {
		req := testRequest()
		req.RuntimeClass = tier
		got := buildDeployment(req, cfg, "web-00001").Spec.Template.Spec.RuntimeClassName
		switch {
		case want == "" && got != nil:
			t.Errorf("tier %q: want no runtimeClassName, got %q", tier, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("tier %q: want runtimeClassName %q, got %v", tier, want, got)
		}
	}
}

func TestLoadConfigFromEnv_RuntimeClasses(t *testing.T) {
	t.Setenv("KUBE_RUNTIME_CLASSES", "kata=kata-qemu")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.RuntimeClasses["kata"] != "kata-qemu" || cfg.RuntimeClasses["gvisor"] != "gvisor" {
		t.Errorf("RuntimeClasses: got %v", cfg.RuntimeClasses)
	}

	t.Setenv("KUBE_RUNTIME_CLASSES", "bogus")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Error("malformed KUBE_RUNTIME_CLASSES: want error")
	}
}

func TestApply_RuntimeClassChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, cs := newTestOrchestrator(t)
	req := &deployment.Request{ID: "web", Image: "nginx:1.27", Hosts: []string{"web.example.com"}, Port: 8080, RuntimeClass: deployment.RuntimeClassGvisor}

	// Missing RuntimeClass → validation error, nothing minted.
	if _, err := o.Apply(ctx, req); !errors.Is(err, apperrors.ErrValidation) {
		t.Fatalf("want validation error, got %v", err)
	}

	// Installed → Apply proceeds and the revision's pod template stamps it.
	_, err := cs.NodeV1().RuntimeClasses().Create(ctx, &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gvisor"},
		Handler:    "runsc",
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	if _, err := o.Apply(ctx, req); err != nil {
		t.Fatalf("apply: %v", err)
	}
	dep, err := cs.AppsV1().Deployments(o.namespace).Get(ctx, "dep-web-00001", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if rc := dep.Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != "gvisor" {
		t.Errorf("runtimeClassName: want gvisor, got %v", rc)
	}
}
