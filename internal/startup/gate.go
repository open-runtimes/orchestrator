// Package startup defines the shared shell startup contract for workload pods.
package startup

import (
	"path/filepath"
	"strconv"
)

const ReadyMarkerDir = ".sidecar"
const ReadyMarkerName = "ready"

const ExecutionMarker = "/dev/orchestrator-started"
const EnvPrepare = "WORKLOAD_PREPARE"
const TimeoutSeconds int64 = 1800

func ReadyMarkerPath(workspace string) string {
	return filepath.Join(workspace, ReadyMarkerDir, ReadyMarkerName)
}

// Arguments carry paths and the command literally; none is interpolated into
// shell source. Exec replaces the gate with the same shell used before gating.
const Script = `trap 'exit 143' TERM
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

func Command(command, workspace string, timeout int64) []string {
	return []string{"/bin/sh", "-c", Script, "orchestrator-gate",
		ReadyMarkerPath(workspace), strconv.FormatInt(timeout*20, 10), command, ExecutionMarker}
}
