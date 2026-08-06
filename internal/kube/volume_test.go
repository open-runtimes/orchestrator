package kube

import (
	"orchestrator/internal/volume"
	"testing"
)

func TestPersistentVolumes_Empty(t *testing.T) {
	vols, mounts := PersistentVolumes(nil)
	if vols != nil || mounts != nil {
		t.Errorf("PersistentVolumes(nil) = %v, %v; want nil, nil", vols, mounts)
	}
}

func TestPersistentVolumes(t *testing.T) {
	in := []volume.Volume{
		{Source: "data-pvc", Path: "/data", ReadOnly: true},
		{Source: "cache-pvc", Path: "/cache", SubPath: "sub"},
	}
	vols, mounts := PersistentVolumes(in)

	if len(vols) != 2 || len(mounts) != 2 {
		t.Fatalf("got %d volumes, %d mounts; want 2, 2", len(vols), len(mounts))
	}

	// Names are index-derived and consistent between volume and mount so two
	// mounts of the same claim can't collide.
	for i := range vols {
		if vols[i].Name != mounts[i].Name {
			t.Errorf("volume/mount name mismatch at %d: %q vs %q", i, vols[i].Name, mounts[i].Name)
		}
	}

	if vols[0].PersistentVolumeClaim == nil || vols[0].PersistentVolumeClaim.ClaimName != "data-pvc" {
		t.Errorf("volume[0] claim = %+v, want data-pvc", vols[0].PersistentVolumeClaim)
	}
	if !vols[0].PersistentVolumeClaim.ReadOnly || !mounts[0].ReadOnly {
		t.Error("volume[0] should be read-only on both PVC source and mount")
	}
	if mounts[0].MountPath != "/data" {
		t.Errorf("mount[0] path = %q, want /data", mounts[0].MountPath)
	}
	if mounts[1].SubPath != "sub" {
		t.Errorf("mount[1] subPath = %q, want sub", mounts[1].SubPath)
	}
}
