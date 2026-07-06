package kubernetes

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
// podName)). The install key lives in the pool-claim-key Secret; a warm pod
// gets its token injected as POOL_CLAIM_TOKEN env at creation, and the claim
// path re-derives it from the pod name — no annotation, nothing readable off
// the pod object.
const (
	claimKeySecretName = "pool-claim-key"
	claimKeySecretKey  = "key"
	claimKeyBytes      = 32
)

// deriveClaimToken computes a pod's claim bearer token from the install key
// and its name.
func deriveClaimToken(key []byte, podName string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(podName))
	return hex.EncodeToString(mac.Sum(nil))
}

// claimKey returns the install key, get-or-creating the pool-claim-key Secret
// on first use and caching it for the process lifetime.
func (o *Orchestrator) claimKey(ctx context.Context) ([]byte, error) {
	o.keyMu.Lock()
	defer o.keyMu.Unlock()
	if o.installKey != nil {
		return o.installKey, nil
	}
	secrets := o.client.CoreV1().Secrets(o.namespace)
	s, err := secrets.Get(ctx, claimKeySecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		fresh := make([]byte, claimKeyBytes)
		if _, err := rand.Read(fresh); err != nil {
			return nil, apperrors.Internal("kubernetes.claimKey", err)
		}
		s, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:   claimKeySecretName,
				Labels: map[string]string{LabelManagedBy: ManagedByValue},
			},
			Data: map[string][]byte{claimKeySecretKey: fresh},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// A racing replica created it first — use theirs.
			s, err = secrets.Get(ctx, claimKeySecretName, metav1.GetOptions{})
		}
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.claimKey", err)
	}
	o.installKey = s.Data[claimKeySecretKey]
	return o.installKey, nil
}
