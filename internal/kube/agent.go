package kube

import (
	"orchestrator/internal/mountagent"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// Mounting is what a pod that mounts needs: a privileged sidecar that mounts
// and propagates outward, or, with the node mount agent
// (docs/design/node-mount-agent.md), an agent-served workspace and a sidecar
// that only asks and keeps file access for the artifacts it still handles.
// Either way the worker receives the mounts with HostToContainer.
type Mounting struct {
	Workspace          corev1.VolumeSource
	SidecarEnv         []corev1.EnvVar
	SidecarPropagation corev1.MountPropagationMode
	SidecarSecurity    *corev1.SecurityContext
}

// NewMounting chooses the agent when agentPort is set. token is the pod's
// workspace token, which the agent receives as a volume attribute and the
// sidecar as env, never the worker. Pass "" for a template several pods share
// and give each pod its own when it is created; the agent refuses an empty
// token, so a missed one fails closed instead of two pods sharing a workspace.
func NewMounting(agentPort int, token string) Mounting {
	if agentPort == 0 {
		return Mounting{
			Workspace:          corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			SidecarPropagation: corev1.MountPropagationBidirectional,
			// Root to open /dev/loop-control, privileged for mount(2).
			SidecarSecurity: MountingSecurityContext(),
		}
	}
	return Mounting{
		Workspace: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver:           mountagent.Driver,
			VolumeAttributes: map[string]string{mountagent.AttributeToken: token},
		}},
		// The agent listens on the node's address, which only the downward API knows.
		SidecarEnv: []corev1.EnvVar{
			{Name: "HOST_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
			{Name: mountagent.EnvEndpoint, Value: "http://$(HOST_IP):" + strconv.Itoa(agentPort)},
			{Name: mountagent.EnvToken, Value: token},
		},
		SidecarPropagation: corev1.MountPropagationHostToContainer,
		SidecarSecurity:    FileAccessSecurityContext(),
	}
}
