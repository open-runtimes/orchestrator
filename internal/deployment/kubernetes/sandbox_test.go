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
	cfg.SandboxRuntimeClasses[deployment.SandboxKata] = "kata-qemu"

	for sandbox, want := range map[string]string{
		"":                       "",
		deployment.SandboxRunc:   "",
		deployment.SandboxGvisor: "gvisor",
		deployment.SandboxKata:   "kata-qemu",
	} {
		req := testRequest()
		req.Sandbox = sandbox
		got := buildDeployment(req, cfg, "web-00001").Spec.Template.Spec.RuntimeClassName
		switch {
		case want == "" && got != nil:
			t.Errorf("sandbox %q: want no runtimeClassName, got %q", sandbox, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("sandbox %q: want runtimeClassName %q, got %v", sandbox, want, got)
		}
	}
}

func TestLoadConfigFromEnv_SandboxRuntimeClasses(t *testing.T) {
	t.Setenv("KUBE_SANDBOX_RUNTIME_CLASSES", "kata=kata-qemu")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.SandboxRuntimeClasses["kata"] != "kata-qemu" || cfg.SandboxRuntimeClasses["gvisor"] != "gvisor" {
		t.Errorf("SandboxRuntimeClasses: got %v", cfg.SandboxRuntimeClasses)
	}

	t.Setenv("KUBE_SANDBOX_RUNTIME_CLASSES", "bogus")
	if _, err := LoadConfigFromEnv(); err == nil {
		t.Error("malformed KUBE_SANDBOX_RUNTIME_CLASSES: want error")
	}
}

func TestApply_SandboxRuntimeClassChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o, cs := newTestOrchestrator(t)
	req := &deployment.Request{ID: "web", Image: "nginx:1.27", Host: "web.example.com", Port: 8080, Sandbox: deployment.SandboxGvisor}

	// Missing RuntimeClass → validation error, nothing minted.
	if err := o.Apply(ctx, req); !errors.Is(err, apperrors.ErrValidation) {
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
	if err := o.Apply(ctx, req); err != nil {
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
