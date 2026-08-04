package warm

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"orchestrator/internal/apperrors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Claim tokens are DERIVED, never stored: token = hex(HMAC-SHA256(installKey,
// podName)). The install key lives in the consumer's claim-key Secret; a warm
// pod gets its token injected as POOL_CLAIM_TOKEN env at creation, and the
// claim path re-derives it from the pod name — no annotation, nothing readable
// off the pod object.
const (
	claimKeySecretKey = "key"
	claimKeyBytes     = 32
)

// deriveClaimToken computes a pod's claim bearer token from the install key
// and its name.
func deriveClaimToken(key []byte, podName string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(podName))
	return hex.EncodeToString(mac.Sum(nil))
}

// claimKey returns the install key, get-or-creating the claim-key Secret on
// first use and caching it for the process lifetime.
func (m *Manager) claimKey(ctx context.Context) ([]byte, error) {
	m.keyMu.Lock()
	defer m.keyMu.Unlock()
	if m.installKey != nil {
		return m.installKey, nil
	}
	secrets := m.client.CoreV1().Secrets(m.cfg.Namespace)
	name := m.cfg.Naming.SecretName
	s, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		fresh := make([]byte, claimKeyBytes)
		if _, err := rand.Read(fresh); err != nil {
			return nil, apperrors.Internal("kubernetes.claimKey", err)
		}
		s, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{LabelManagedBy: m.cfg.Naming.ManagedBy},
			},
			Data: map[string][]byte{claimKeySecretKey: fresh},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// A racing replica created it first — use theirs.
			s, err = secrets.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.claimKey", err)
	}
	m.installKey = s.Data[claimKeySecretKey]
	return m.installKey, nil
}
