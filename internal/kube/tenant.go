package kube

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TenantNamespace resolves a tenant to its namespace: the base workload
// namespace when tenant is empty (the shared default), otherwise
// "{base}-{tenant}". Tenants are validated as RFC-1123 labels by the API, so
// the derived name is always a valid namespace.
func TenantNamespace(base, tenant string) string {
	if tenant == "" {
		return base
	}
	return base + "-" + tenant
}

// EnsureTenantNamespace creates the tenant namespace (with restricted Pod
// Security admission labels) and the given ServiceAccount inside it if they
// are missing — the on-demand provisioning behind a per-request tenant. The
// base namespace is never touched (the chart owns it); only derived tenant
// namespaces are ensured. Idempotent and safe to call on every placement.
//
// Deliberately minimal: no per-namespace NetworkPolicy or ResourceQuota —
// network isolation comes from a cluster-wide policy and resource control
// from pod limits, so a tenant namespace is cheap to spin up.
func EnsureTenantNamespace(ctx context.Context, client kubernetes.Interface, namespace, serviceAccount string, labels map[string]string) error {
	nsLabels := map[string]string{
		"pod-security.kubernetes.io/enforce": "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
		"pod-security.kubernetes.io/warn":    "restricted",
	}
	maps.Copy(nsLabels, labels)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: nsLabels}}
	if _, err := client.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure namespace %s: %w", namespace, err)
	}

	if serviceAccount != "" {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccount, Namespace: namespace}}
		if _, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("ensure serviceaccount %s/%s: %w", namespace, serviceAccount, err)
		}
	}
	return nil
}
