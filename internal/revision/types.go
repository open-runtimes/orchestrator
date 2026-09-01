// Package revision defines the Kubernetes API contract used by the direct-pod
// deployment backend. A Revision is one independently scalable, immutable
// member of an orchestrator Deployment.
package revision

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"orchestrator/internal/workload"
)

const (
	Group   = "orchestrator.open-runtimes.io"
	Version = "v1alpha1"
	Kind    = "Revision"
)

func Resource() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: Group, Version: Version, Resource: "revisions"}
}

type Spec struct {
	Replicas            int32                   `json:"replicas"`
	ReadyTimeoutSeconds int32                   `json:"readyTimeoutSeconds,omitempty"`
	Template            *corev1.PodTemplateSpec `json:"template,omitempty"`
	// AcquisitionKey is the canonical fixed-shape fingerprint used to resolve
	// current operator pools for each missing replica slot. Pool is retained
	// only to read pool-selected Revisions created by the previous design.
	AcquisitionKey string                 `json:"acquisitionKey,omitempty"`
	Pool           string                 `json:"pool,omitempty"`
	Claim          *workload.ClaimRequest `json:"claim,omitempty"`
}

type Status struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Replicas           int32              `json:"replicas,omitempty"`
	ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type Revision struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              Spec   `json:"spec"`
	Status            Status `json:"status,omitzero"`
}

type List struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Revision `json:"items"`
}

func APIVersion() string { return Group + "/" + Version }
