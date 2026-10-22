package moby

import (
	"context"
	"errors"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/volume"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

// daemonVersion stands in for a client that negotiated version with the daemon.
type daemonVersion string

func (daemonVersion) NegotiateAPIVersion(context.Context) {}
func (v daemonVersion) ClientVersion() string             { return string(v) }

func TestVolumeMounts(t *testing.T) {
	t.Parallel()
	mounts, err := Mounts(context.Background(), daemonVersion("1.47"), []volume.Volume{
		{Source: "data", Path: "/data", ReadOnly: true},
		{Source: "cache", Path: "/cache", SubPath: "sub"},
	})
	if err != nil {
		t.Fatal(err)
	}
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

// A daemon older than API 1.45 ignores a subPath and mounts the whole volume;
// the request is refused instead of widened.
func TestVolumeMounts_SubPathNeedsAPI145(t *testing.T) {
	t.Parallel()
	vols := []volume.Volume{
		{Source: "data", Path: "/data"},
		{Source: "builds", Path: "/builds", SubPath: "app-1/dep-1"},
	}

	for _, v := range []string{"1.41", "1.44"} {
		if _, err := Mounts(context.Background(), daemonVersion(v), vols); !errors.Is(err, apperrors.ErrValidation) {
			t.Errorf("API %s: Mounts() error = %v, want a validation error", v, err)
		}
	}
	for _, v := range []string{"1.45", "1.51"} {
		if _, err := Mounts(context.Background(), daemonVersion(v), vols); err != nil {
			t.Errorf("API %s: Mounts() error = %v", v, err)
		}
	}
	// Without a subPath there is nothing an old daemon could widen.
	if _, err := Mounts(context.Background(), daemonVersion("1.41"), vols[:1]); err != nil {
		t.Errorf("API 1.41 without subPath: Mounts() error = %v", err)
	}
}
