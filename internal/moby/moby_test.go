package moby

import (
	"orchestrator/internal/volume"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestVolumeMounts(t *testing.T) {
	t.Parallel()
	mounts := Mounts([]volume.Volume{
		{Source: "data", Path: "/data", ReadOnly: true},
		{Source: "cache", Path: "/cache", SubPath: "sub"},
	})
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
	if mounts[0].Type != mount.TypeVolume || mounts[0].Source != "data" || mounts[0].Target != "/data" || !mounts[0].ReadOnly {
		t.Errorf("mount[0] = %+v", mounts[0])
	}
	if mounts[0].VolumeOptions != nil {
		t.Error("mount[0] should have no VolumeOptions without a subPath")
	}
	if mounts[1].VolumeOptions == nil || mounts[1].VolumeOptions.Subpath != "sub" {
		t.Errorf("mount[1] subPath = %+v, want sub", mounts[1].VolumeOptions)
	}
}
