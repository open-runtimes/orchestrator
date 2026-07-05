// Package kubernetes implements the deployment.Orchestrator interface using
// the Kubernetes API. Each deployment is an apps/v1.Deployment plus a
// ClusterIP Service in a configured namespace. Kubernetes is the source of
// truth — Status, List, Spec, and Endpoints derive from it live, so any
// replica can serve any request and a restart loses nothing.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/kube"
	"orchestrator/internal/proxy"
	"orchestrator/pkg/deployment"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Orchestrator implements deployment.Orchestrator using Kubernetes.
type Orchestrator struct {
	client    kubernetes.Interface
	namespace string
	cfg       Config
}

// NewOrchestrator creates a Kubernetes deployment orchestrator.
func NewOrchestrator(ctx context.Context, cfg Config) (*Orchestrator, error) {
	cfg.applyDefaults()
	cs, err := kube.NewClient(cfg.Kubeconfig, cfg.Context, nil)
	if err != nil {
		return nil, err
	}
	return &Orchestrator{client: cs, namespace: cfg.Namespace, cfg: cfg}, nil
}

// Start surveys pre-existing managed deployments. Kubernetes reconciles them
// autonomously, so there is nothing to resume — this just confirms API access
// and logs what the backend is already running.
func (o *Orchestrator) Start(ctx context.Context) error {
	deps, err := o.client.AppsV1().Deployments(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return apperrors.Internal("kubernetes.listDeployments", err)
	}
	slog.Info("Deployment orchestrator started", "namespace", o.namespace, "deployments", len(deps.Items))
	return nil
}

// Apply creates the deployment or replaces its spec in place. The canonical
// JSON of the request is stored in the deployment.spec annotation; when the
// incoming request marshals to the same JSON, the workload is untouched (no
// rollout). The fronting Service is ensured on every call — its shape never
// changes, so create-if-missing also heals a partial earlier Apply.
func (o *Orchestrator) Apply(ctx context.Context, req *deployment.Request) error {
	specJSON, err := json.Marshal(req)
	if err != nil {
		return apperrors.Internal("kubernetes.marshalSpec", err)
	}
	desired := buildDeployment(req, o.cfg, string(specJSON))

	deployments := o.client.AppsV1().Deployments(o.namespace)
	existing, err := deployments.Get(ctx, desired.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := deployments.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
			return apperrors.Internal("kubernetes.createDeployment", err)
		}
	case err != nil:
		return apperrors.Internal("kubernetes.getDeployment", err)
	case existing.Annotations[AnnotationSpec] == string(specJSON):
		// Identical spec — leave the workload alone.
	default:
		desired.ResourceVersion = existing.ResourceVersion
		if _, err := deployments.Update(ctx, desired, metav1.UpdateOptions{}); err != nil {
			return apperrors.Internal("kubernetes.updateDeployment", err)
		}
	}

	return o.ensureService(ctx, req)
}

func (o *Orchestrator) ensureService(ctx context.Context, req *deployment.Request) error {
	_, err := o.client.CoreV1().Services(o.namespace).Create(ctx, buildService(req), metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return apperrors.Internal("kubernetes.createService", err)
	}
	return nil
}

// SetTraffic reconciles the deployment's HTTPRoute weights. Placeholder until
// the Phase 3 revision rework lands in this package.
func (o *Orchestrator) SetTraffic(ctx context.Context, id string, targets []deployment.Target) error {
	if _, err := o.Spec(ctx, id); err != nil {
		return err
	}
	return apperrors.Validation("traffic", "traffic splitting arrives with the revision rework")
}

// Scale sets the replica count via the scale subresource — the same write the
// activator's cold raise and the idle-to-zero loop perform, so they can't
// conflict with a concurrent Apply (which never touches a live spec.replicas
// unless the spec changed).
func (o *Orchestrator) Scale(ctx context.Context, id string, replicas int) error {
	if replicas < 0 {
		replicas = 0
	}
	deployments := o.client.AppsV1().Deployments(o.namespace)
	scale, err := deployments.GetScale(ctx, deploymentNameFor(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return apperrors.Internal("kubernetes.getScale", err)
	}
	if scale.Spec.Replicas == int32(replicas) {
		return nil
	}
	scale.Spec.Replicas = int32(replicas)
	if _, err := deployments.UpdateScale(ctx, deploymentNameFor(id), scale, metav1.UpdateOptions{}); err != nil {
		return apperrors.Internal("kubernetes.updateScale", err)
	}
	return nil
}

// Delete tears down the deployment's Service and apps/v1.Deployment (foreground
// propagation, so pods are gone before the Deployment object is). NotFound only
// when neither object existed.
func (o *Orchestrator) Delete(ctx context.Context, id string) error {
	name := deploymentNameFor(id)

	svcErr := o.client.CoreV1().Services(o.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if svcErr != nil && !apierrors.IsNotFound(svcErr) {
		return apperrors.Internal("kubernetes.deleteService", svcErr)
	}

	prop := metav1.DeletePropagationForeground
	depErr := o.client.AppsV1().Deployments(o.namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if depErr != nil && !apierrors.IsNotFound(depErr) {
		return apperrors.Internal("kubernetes.deleteDeployment", depErr)
	}

	if apierrors.IsNotFound(svcErr) && apierrors.IsNotFound(depErr) {
		return apperrors.NotFound("deployment", id)
	}
	return nil
}

// Spec reconstructs the last-applied request from the deployment.spec annotation.
func (o *Orchestrator) Spec(ctx context.Context, id string) (*deployment.Request, error) {
	dep, err := o.client.AppsV1().Deployments(o.namespace).Get(ctx, deploymentNameFor(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.getDeployment", err)
	}
	raw := dep.Annotations[AnnotationSpec]
	if raw == "" {
		return nil, apperrors.Internal("kubernetes.readSpec", fmt.Errorf("deployment %s missing %s annotation", dep.Name, AnnotationSpec))
	}
	var req deployment.Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return nil, apperrors.Internal("kubernetes.unmarshalSpec", err)
	}
	return &req, nil
}

// Endpoints returns the proxy data-port URL of every ready pod — the
// activator's direct forward targets.
func (o *Orchestrator) Endpoints(ctx context.Context, id string) ([]*url.URL, error) {
	pods, err := o.client.CoreV1().Pods(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelDeploymentID + "=" + id,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listPods", err)
	}
	var endpoints []*url.URL
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !isPodReady(pod) || pod.Status.PodIP == "" {
			continue
		}
		endpoints = append(endpoints, &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(proxy.DefaultProxyPort)),
		})
	}
	return endpoints, nil
}

// Status returns the deployment's current state, derived live from the
// apps/v1.Deployment.
func (o *Orchestrator) Status(ctx context.Context, id string) (*deployment.StatusResponse, error) {
	dep, err := o.client.AppsV1().Deployments(o.namespace).Get(ctx, deploymentNameFor(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, apperrors.NotFound("deployment", id)
	}
	if err != nil {
		return nil, apperrors.Internal("kubernetes.getDeployment", err)
	}
	status := deriveStatus(dep)
	return &status, nil
}

// List returns the status of all managed deployments.
func (o *Orchestrator) List(ctx context.Context) ([]deployment.StatusResponse, error) {
	deps, err := o.client.AppsV1().Deployments(o.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelManagedBy + "=" + ManagedByValue,
	})
	if err != nil {
		return nil, apperrors.Internal("kubernetes.listDeployments", err)
	}
	statuses := make([]deployment.StatusResponse, 0, len(deps.Items))
	for i := range deps.Items {
		if deps.Items[i].Labels[LabelDeploymentID] == "" {
			slog.Warn("Skipping managed deployment without a deployment.id label", "name", deps.Items[i].Name)
			continue
		}
		statuses = append(statuses, deriveStatus(&deps.Items[i]))
	}
	return statuses, nil
}

// Ready checks that the K8s API server is reachable.
func (o *Orchestrator) Ready(ctx context.Context) error {
	_, err := o.client.Discovery().ServerVersion()
	return err
}

// Close releases orchestrator resources. Running deployments are NOT stopped —
// Kubernetes keeps serving them independently.
func (o *Orchestrator) Close() error {
	return nil
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Verify Orchestrator implements deployment.Orchestrator.
var _ deployment.Orchestrator = (*Orchestrator)(nil)
