package kube

import (
	"orchestrator/internal/mountagent"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func agentTemplate() corev1.PodSpec {
	volume, env := AgentWorkspace(9878, "")
	return corev1.PodSpec{
		Volumes:        []corev1.Volume{{Name: "workspace", VolumeSource: volume}},
		InitContainers: []corev1.Container{{Name: "proxy", Env: env}},
		Containers:     []corev1.Container{{Name: "worker"}},
	}
}

func tokens(spec corev1.PodSpec) (volume, env string) {
	volume = spec.Volumes[0].CSI.VolumeAttributes[mountagent.AttributeToken]
	for _, e := range spec.InitContainers[0].Env {
		if e.Name == mountagent.EnvToken {
			env = e.Value
		}
	}
	return volume, env
}

// Pods built from one template must each get their own token: the agent binds
// a token to the first workspace that presents it and refuses the rest.
func TestStampAgentTokenGivesEachPodItsOwn(t *testing.T) {
	t.Parallel()
	template := agentTemplate()
	first, second := *template.DeepCopy(), *template.DeepCopy()
	StampAgentToken(&first)
	StampAgentToken(&second)

	v1, e1 := tokens(first)
	v2, e2 := tokens(second)
	if len(v1) < 26 || v1 != e1 {
		t.Fatalf("first pod: volume token %q, sidecar token %q", v1, e1)
	}
	if v2 != e2 {
		t.Fatalf("second pod: volume token %q, sidecar token %q", v2, e2)
	}
	if v1 == v2 {
		t.Fatal("two pods from one template share a workspace token")
	}
	if v, _ := tokens(template); v != "" {
		t.Fatal("stamping a copy changed the shared template")
	}
}

func TestStampAgentTokenLeavesOtherPodsAlone(t *testing.T) {
	t.Parallel()
	spec := corev1.PodSpec{
		Volumes:    []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		Containers: []corev1.Container{{Name: "worker", Env: []corev1.EnvVar{{Name: "A", Value: "b"}}}},
	}
	StampAgentToken(&spec)
	if spec.Volumes[0].CSI != nil || len(spec.Containers[0].Env) != 1 {
		t.Fatalf("a pod without an agent workspace was changed: %+v", spec)
	}
}

// $(HOST_IP) only expands from a variable defined earlier in the list.
func TestAgentWorkspaceDefinesHostIPFirst(t *testing.T) {
	t.Parallel()
	_, env := AgentWorkspace(9878, "t")
	if env[0].Name != "HOST_IP" || env[0].ValueFrom.FieldRef.FieldPath != "status.hostIP" || env[1].Value != "http://$(HOST_IP):9878" {
		t.Fatalf("env = %+v", env)
	}
}
