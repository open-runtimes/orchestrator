//go:build linux

package mount

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestOverlayMountFlagsKeepDevicesAndSetuidOut(t *testing.T) {
	for name, flag := range map[string]uintptr{"nodev": unix.MS_NODEV, "nosuid": unix.MS_NOSUID} {
		if overlayMountFlags&flag == 0 {
			t.Errorf("overlay mounts without %s", name)
		}
	}
}
