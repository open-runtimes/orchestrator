package config

import "testing"

// The workspace path travels from the backend to every process of a workload in
// one variable. A process told nothing must land on the same default the API
// hands out, or the backend mounts the volume in one place and the worker looks
// in another.
func TestWorkspace(t *testing.T) {
	if got := Workspace(); got != DefaultWorkspace {
		t.Errorf("with nothing set: want %q, got %q", DefaultWorkspace, got)
	}

	t.Setenv(EnvSharedVolume, "/mnt/scratch")
	if got := Workspace(); got != "/mnt/scratch" {
		t.Errorf("with the variable set: want /mnt/scratch, got %q", got)
	}
}
