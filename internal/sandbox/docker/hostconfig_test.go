package docker

import (
	"slices"
	"testing"
)

// A sandbox runs the user's commands: no raw sockets on the shared bridge and
// no privilege gain through setuid binaries.
func TestWorkerHostConfig_Hardened(t *testing.T) {
	hc := workerHostConfig(nil, 1, 512, nil)
	if !slices.Contains(hc.CapDrop, "NET_RAW") || !slices.Contains(hc.SecurityOpt, "no-new-privileges") || hc.Privileged {
		t.Errorf("CapDrop = %v, SecurityOpt = %v, Privileged = %v; want NET_RAW dropped and no-new-privileges", hc.CapDrop, hc.SecurityOpt, hc.Privileged)
	}
}
