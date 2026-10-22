package kube

import (
	"crypto/rand"
	"orchestrator/internal/mountagent"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// AgentWorkspace is a workspace the node mount agent serves
// (docs/design/node-mount-agent.md): an inline CSI volume carrying the pod's
// token, and the env that points the pod's sidecar at the agent on its node
// with the same token. Pass a fresh token for a pod built on its own; pass ""
// for a template several pods share, and give each pod its own with
// StampAgentToken. The agent refuses an empty token, so a missed stamp fails
// closed instead of two pods sharing a workspace.
func AgentWorkspace(port int, token string) (corev1.VolumeSource, []corev1.EnvVar) {
	volume := corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
		Driver:           mountagent.Driver,
		VolumeAttributes: map[string]string{mountagent.AttributeToken: token},
	}}
	// The agent listens on the node's address, which only the downward API knows.
	env := []corev1.EnvVar{
		{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		{Name: mountagent.EnvEndpoint, Value: "http://$(HOST_IP):" + strconv.Itoa(port)},
		{Name: mountagent.EnvToken, Value: token},
	}
	return volume, env
}

// StampAgentToken gives a pod built from a shared template its own workspace
// token, in the agent volume and the sidecar env alike. A spec without an
// agent workspace is left untouched.
func StampAgentToken(spec *corev1.PodSpec) {
	token := ""
	for i := range spec.Volumes {
		if csi := spec.Volumes[i].CSI; csi != nil && csi.Driver == mountagent.Driver {
			token = rand.Text() // 130 random bits
			csi.VolumeAttributes = map[string]string{mountagent.AttributeToken: token}
		}
	}
	if token == "" {
		return
	}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range containers {
			for j := range containers[i].Env {
				if containers[i].Env[j].Name == mountagent.EnvToken {
					containers[i].Env[j].Value = token
				}
			}
		}
	}
}
