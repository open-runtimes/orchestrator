//go:build linux

package sidecar

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Run in an isolated mount namespace with CAP_SYS_ADMIN. CI invokes this test
// explicitly with sudo unshare; ordinary unprivileged test runs skip it.
func TestKernelMounterBusyCleanup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "code")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Model the volume mount underneath the artifact overlay. Repeated cleanup
	// must not accidentally unmount this Kubernetes-owned mount.
	if err := unix.Mount(target, target, "", unix.MS_BIND, ""); err != nil {
		if errors.Is(err, unix.EPERM) && os.Getenv("REQUIRE_MOUNT_TESTS") != "1" {
			t.Skip("requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })
	lower := overlayLower(target)
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lower, "data"), []byte("still readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	mounter := kernelMounter{}
	if err := mounter.Mount(lower, target, MountOpts{Writable: true, SourceDir: true, SizeMiB: 16}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Unmount(overlayScratch(target), unix.MNT_DETACH)
		_ = unix.Unmount(lower, unix.MNT_DETACH)
	})
	reader, err := os.Open(filepath.Join(target, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := mounter.Unmount(target); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 32)
	n, err := reader.Read(data)
	if err != nil || string(data[:n]) != "still readable" {
		t.Fatalf("detached reader: %q, %v", data[:n], err)
	}
	// The holder is deliberately still open here. Mount disappearance must not
	// depend on the reader releasing its reference later.
	if err := mounter.Unmount(target); err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for line := range strings.SplitSeq(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		path := unescapeMountPath(fields[4])
		if path == target {
			count++
		}
		if path == lower || path == overlayScratch(target) {
			t.Fatalf("artifact mount remains: %s", line)
		}
	}
	if count != 1 {
		t.Fatalf("wanted only the backing volume, found %d mounts at target", count)
	}
}
