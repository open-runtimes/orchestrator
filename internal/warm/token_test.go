package warm

import (
	"orchestrator/internal/workload"
	"orchestrator/pkg/pool"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Secret material at rest (docs/operations.md): claim tokens are derived from
// an install key that never leaves its Secret, and never stored on a pod.

// testInstallKey is a fixed claim-token HMAC key for deterministic tests.
var testInstallKey = []byte("0123456789abcdef0123456789abcdef")

func getPod(t *testing.T, cs *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNS).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	return pod
}

func TestDeriveClaimToken_DeterministicPerPod(t *testing.T) {
	t.Parallel()
	a1 := deriveClaimToken(testInstallKey, "pool-std-aaaaa")
	a2 := deriveClaimToken(testInstallKey, "pool-std-aaaaa")
	b := deriveClaimToken(testInstallKey, "pool-std-bbbbb")
	other := deriveClaimToken([]byte("another-install-key-32-bytes-xx!"), "pool-std-aaaaa")
	if a1 != a2 {
		t.Error("token must be deterministic for (key, podName)")
	}
	if a1 == b {
		t.Error("tokens must differ across pods")
	}
	if a1 == other {
		t.Error("tokens must differ across install keys")
	}
	if len(a1) != 64 { // hex(HMAC-SHA256)
		t.Errorf("token length: want 64 hex chars, got %d", len(a1))
	}
}

func TestClaimKey_GetOrCreateIdempotent(t *testing.T) {
	t.Parallel()
	m, cs, _ := newTestManager(t, testPool("std"))
	m.installKey = nil // exercise the get-or-create path

	first, err := m.claimKey(t.Context())
	if err != nil {
		t.Fatalf("claimKey: %v", err)
	}
	if len(first) != claimKeyBytes {
		t.Fatalf("key length: want %d, got %d", claimKeyBytes, len(first))
	}
	secret, err := cs.CoreV1().Secrets(testNS).Get(t.Context(), testNaming.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected the claim-key Secret: %v", err)
	}
	if string(secret.Data[claimKeySecretKey]) != string(first) {
		t.Error("cached key must match the stored Secret")
	}

	// A second replica against the same cluster adopts the same key.
	m2 := New(cs, []pool.Pool{testPool("std")}, Config{Namespace: testNS, Naming: testNaming})
	second, err := m2.claimKey(t.Context())
	if err != nil {
		t.Fatalf("claimKey (second): %v", err)
	}
	if string(second) != string(first) {
		t.Error("get-or-create must be idempotent across replicas")
	}
}

func TestCreate_InjectsDerivedTokenNoAnnotation(t *testing.T) {
	t.Parallel()
	m, cs, _ := newTestManager(t, testPool("std"))

	created, err := m.Create(t.Context(), m.Pool("std"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pod := getPod(t, cs, created.Name)
	if _, ok := pod.Annotations["pool.claim-token"]; ok {
		t.Error("claim token must never be annotated on the pod")
	}
	want := deriveClaimToken(testInstallKey, pod.Name)
	found := false
	for _, c := range pod.Spec.InitContainers {
		if c.Name != ContainerProxy {
			continue
		}
		for _, env := range c.Env {
			if env.Name == workload.EnvClaimToken {
				found = true
				if env.Value != want {
					t.Errorf("POOL_CLAIM_TOKEN: want the derived token, got %q", env.Value)
				}
			}
		}
	}
	if !found {
		t.Error("sidecar must still receive POOL_CLAIM_TOKEN env")
	}
}
