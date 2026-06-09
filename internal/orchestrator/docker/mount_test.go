package docker

import (
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestWorkspaceMount_Volume(t *testing.T) {
	h := dockerHandle{volumeName: "job-1-workspace"}
	m := workspaceMount(h, "/workspace", mount.PropagationRSlave)

	if m.Type != mount.TypeVolume {
		t.Errorf("Type: want volume, got %s", m.Type)
	}
	if m.Source != "job-1-workspace" || m.Target != "/workspace" {
		t.Errorf("unexpected source/target: %s -> %s", m.Source, m.Target)
	}
	if m.BindOptions != nil {
		t.Error("named volume should not carry bind options")
	}
}

func TestWorkspaceMount_BindWithPropagation(t *testing.T) {
	h := dockerHandle{hostDir: "/tmp/job-1-ws"}
	m := workspaceMount(h, "/workspace", mount.PropagationRShared)

	if m.Type != mount.TypeBind {
		t.Errorf("Type: want bind, got %s", m.Type)
	}
	if m.Source != "/tmp/job-1-ws" || m.Target != "/workspace" {
		t.Errorf("unexpected source/target: %s -> %s", m.Source, m.Target)
	}
	if m.BindOptions == nil || m.BindOptions.Propagation != mount.PropagationRShared {
		t.Errorf("want rshared bind propagation, got %+v", m.BindOptions)
	}
}
