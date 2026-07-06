package kube

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTenantNamespace(t *testing.T) {
	t.Parallel()
	if got := TenantNamespace("orchestrator", ""); got != "orchestrator" {
		t.Errorf("empty tenant = %q, want base", got)
	}
	if got := TenantNamespace("orchestrator", "acme"); got != "orchestrator-acme" {
		t.Errorf("tenant acme = %q, want orchestrator-acme", got)
	}
}

func TestEnsureTenantNamespace(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()

	// Creates namespace (restricted PSA) + serviceaccount.
	if err := EnsureTenantNamespace(t.Context(), client, "orchestrator-acme", "job-sidecar", nil); err != nil {
		t.Fatal(err)
	}
	ns, err := client.CoreV1().Namespaces().Get(t.Context(), "orchestrator-acme", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("PSA enforce = %q, want restricted", ns.Labels["pod-security.kubernetes.io/enforce"])
	}
	if _, err := client.CoreV1().ServiceAccounts("orchestrator-acme").Get(t.Context(), "job-sidecar", metav1.GetOptions{}); err != nil {
		t.Errorf("serviceaccount not created: %v", err)
	}

	// Idempotent — a second call over the existing objects is a no-op.
	if err := EnsureTenantNamespace(t.Context(), client, "orchestrator-acme", "job-sidecar", nil); err != nil {
		t.Errorf("second ensure: %v", err)
	}
}
