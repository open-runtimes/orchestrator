package docker

import (
	"slices"
	"testing"
)

// A replica runs the user's command: no raw sockets on the shared bridge and
// no privilege gain through setuid binaries.
func TestWorkerHostConfig_Hardened(t *testing.T) {
	hc := workerHostConfig(nil, 0.5, 256)
	if !slices.Contains(hc.CapDrop, "NET_RAW") || !slices.Contains(hc.SecurityOpt, "no-new-privileges") || hc.Privileged {
		t.Errorf("CapDrop = %v, SecurityOpt = %v, Privileged = %v; want NET_RAW dropped and no-new-privileges", hc.CapDrop, hc.SecurityOpt, hc.Privileged)
	}
	if hc.Memory != 256*1024*1024 || hc.NanoCPUs != 5e8 {
		t.Errorf("Memory = %d, NanoCPUs = %d; want the requested resources", hc.Memory, hc.NanoCPUs)
	}
}
