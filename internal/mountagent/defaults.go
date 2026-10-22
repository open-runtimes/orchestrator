package mountagent

// Defaults shared by the agent and the mapper that targets it. Both are
// overridable (KUBE_MOUNT_AGENT_DRIVER, KUBE_MOUNT_AGENT_PORT on the services;
// the chart's mountAgent values on the agent) and must agree.
const (
	DefaultDriver = "mount.open-runtimes.dev"
	DefaultPort   = 9878
)
