package kubernetes

import (
	"orchestrator/internal/sidecar"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	annotationStartupGate = "job.startup-gate"
	startupGateVersion    = "shell-v1"
	// Kubernetes creates this writable file before container start and retains
	// its contents in terminated container status. It is private to the gate.
	executionMarker = "/dev/orchestrator-started"
	executionPrefix = "orchestrator-started:"
)

// Arguments carry paths and the command literally; none is interpolated into
// shell source. Exec replaces the gate with the same shell used before gating.
const startupGateScript = `trap 'exit 143' TERM
trap 'exit 130' INT
i=0
while [ ! -f "$1" ]; do
  if [ "$i" -ge "$2" ]; then
    echo 'workload startup gate timed out' >&2
    exit 125
  fi
  i=$((i+1))
  sleep 0.05 &
  wait "$!" || exit 125
done
started=$(date +%s) || exit 125
printf 'orchestrator-started:%s\n' "$started" > "$4" || exit 125
exec /bin/sh -c "$3"
`

func gatedCommand(command, workspace string, timeout int64) []string {
	return []string{"/bin/sh", "-c", startupGateScript, "orchestrator-gate",
		sidecar.ReadyMarkerPath(workspace), strconv.FormatInt(timeout*20, 10), command, executionMarker}
}

func gatedPod(pod *corev1.Pod) bool {
	return pod.Annotations[annotationStartupGate] == startupGateVersion
}

func executionStart(worker *corev1.ContainerStatus) time.Time {
	if worker == nil || worker.State.Terminated == nil {
		return time.Time{}
	}
	message := strings.TrimSpace(worker.State.Terminated.Message)
	if !strings.HasPrefix(message, executionPrefix) {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(strings.TrimPrefix(message, executionPrefix), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

// StartupProbe reports release while running. The termination message covers
// fast commands that exit before kubelet ever observes the probe succeeding.
func workloadReleased(pod *corev1.Pod, worker *corev1.ContainerStatus) bool {
	if worker == nil {
		return false
	}
	if !gatedPod(pod) {
		return worker.State.Running != nil || worker.State.Terminated != nil
	}
	return !executionStart(worker).IsZero() || (worker.Started != nil && *worker.Started)
}
