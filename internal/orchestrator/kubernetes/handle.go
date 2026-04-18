package kubernetes

// kubernetesHandle carries the K8s infrastructure identifiers for a running job.
type kubernetesHandle struct {
	namespace string
	jobName   string
}
