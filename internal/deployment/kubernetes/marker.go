package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"orchestrator/internal/apperrors"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Marker ConfigMap data keys. The marker (named dep-{id}) is the one object
// that always exists for a deployment — the identity anchor Spec(), List(),
// and host resolution read; no marker means NotFound. The head revision's
// spec JSON lives on the companion dep-{id} Secret (it carries the callback
// signing key), never on the marker.
const (
	markerKeyLatestRevision = "latestRevision" // newest minted revision name
	markerKeyLastReady      = "lastReady"      // last revision the auto-cut shifted traffic to
	markerKeyTrafficMode    = "trafficMode"    // auto | manual

	trafficModeAuto   = "auto"
	trafficModeManual = "manual"
)

// specSecretKey is the data key on the per-deployment dep-{id} Secret
// holding the head revision's canonical spec JSON.
const specSecretKey = "spec"

// marker is the decoded per-deployment marker ConfigMap.
type marker struct {
	ID             string
	Hosts          []string // from the deployment.host annotation (comma-separated)
	LatestRevision string
	LastReady      string
	TrafficMode    string
	Deleting       bool // DeletionTimestamp set on the ConfigMap
}

func markerFromConfigMap(cm *corev1.ConfigMap) marker {
	return marker{
		ID:             cm.Labels[LabelDeploymentID],
		Hosts:          splitHosts(cm.Annotations[AnnotationHost]),
		LatestRevision: cm.Data[markerKeyLatestRevision],
		LastReady:      cm.Data[markerKeyLastReady],
		TrafficMode:    cm.Data[markerKeyTrafficMode],
		Deleting:       cm.DeletionTimestamp != nil,
	}
}

func (m marker) configMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: objectNameFor(m.ID),
			Labels: map[string]string{
				LabelManagedBy:    ManagedByValue,
				LabelDeploymentID: m.ID,
			},
			Annotations: map[string]string{AnnotationHost: strings.Join(m.Hosts, ",")},
		},
		Data: map[string]string{
			markerKeyLatestRevision: m.LatestRevision,
			markerKeyLastReady:      m.LastReady,
			markerKeyTrafficMode:    m.TrafficMode,
		},
	}
}

// --- spec Secret ---

// errSpecMissing marks a deployment whose marker exists but whose dep-{id}
// spec Secret is gone — Apply heals it, Spec reports it.
var errSpecMissing = errors.New("spec secret missing")

// specSecret builds the dep-{id} Secret holding the head spec JSON.
func specSecret(id, specJSON string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: objectNameFor(id),
			Labels: map[string]string{
				LabelManagedBy:    ManagedByValue,
				LabelDeploymentID: id,
			},
		},
		Data: map[string][]byte{specSecretKey: []byte(specJSON)},
	}
}

// getSpecJSON reads the head spec JSON off the dep-{id} Secret; a missing
// Secret is errSpecMissing.
func (o *Orchestrator) getSpecJSON(ctx context.Context, id string) (string, error) {
	s, err := o.client.CoreV1().Secrets(o.namespace).Get(ctx, objectNameFor(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", errSpecMissing
	}
	if err != nil {
		return "", apperrors.Internal("kubernetes.getSpecSecret", err)
	}
	return string(s.Data[specSecretKey]), nil
}

// writeSpecJSON creates or replaces the dep-{id} spec Secret.
func (o *Orchestrator) writeSpecJSON(ctx context.Context, id, specJSON string) error {
	secrets := o.client.CoreV1().Secrets(o.namespace)
	_, err := secrets.Create(ctx, specSecret(id, specJSON), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = secrets.Update(ctx, specSecret(id, specJSON), metav1.UpdateOptions{})
	}
	if err != nil {
		return apperrors.Internal("kubernetes.writeSpecSecret", err)
	}
	return nil
}

// deleteSpecSecret removes the dep-{id} spec Secret, tolerating already-gone.
func (o *Orchestrator) deleteSpecSecret(ctx context.Context, id string) error {
	err := o.client.CoreV1().Secrets(o.namespace).Delete(ctx, objectNameFor(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return apperrors.Internal("kubernetes.deleteSpecSecret", err)
	}
	return nil
}

// getMarker reads the deployment's marker; a missing marker is the
// deployment's NotFound.
func (o *Orchestrator) getMarker(ctx context.Context, id string) (marker, error) {
	cm, err := o.client.CoreV1().ConfigMaps(o.namespace).Get(ctx, objectNameFor(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return marker{}, apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return marker{}, apperrors.Internal("kubernetes.getMarker", err)
	}
	return markerFromConfigMap(cm), nil
}

func (o *Orchestrator) createMarker(ctx context.Context, m marker) error {
	if _, err := o.client.CoreV1().ConfigMaps(o.namespace).Create(ctx, m.configMap(), metav1.CreateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.createMarker", err)
	}
	return nil
}

// updateMarker read-modify-writes the marker so concurrent writers (Apply,
// the rollout reconciler, SetTraffic) never clobber fields they don't own.
func (o *Orchestrator) updateMarker(ctx context.Context, id string, mutate func(*marker)) error {
	cms := o.client.CoreV1().ConfigMaps(o.namespace)
	cm, err := cms.Get(ctx, objectNameFor(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getMarker", err)
	}
	m := markerFromConfigMap(cm)
	mutate(&m)
	desired := m.configMap()
	desired.ResourceVersion = cm.ResourceVersion
	if _, err := cms.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.updateMarker", err)
	}
	return nil
}

// --- revision naming ---

// revisionName renders the {id}-{%05d} revision name (web-00001).
func revisionName(id string, n int) string {
	return fmt.Sprintf("%s-%05d", id, n)
}

// revisionNumber parses the trailing sequence number from a revision name;
// 0 when the name doesn't carry one.
func revisionNumber(rev string) int {
	i := strings.LastIndex(rev, "-")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(rev[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// splitHosts decodes the comma-joined hosts annotation (hosts are RFC-1123
// subdomains, so commas can never appear inside one).
func splitHosts(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}
