//go:build linux

package testutil

import (
	"errors"
	"orchestrator/internal/squashfs"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// RequireMountPrivilege skips a test that needs CAP_SYS_ADMIN in a private
// mount namespace, unless REQUIRE_MOUNT_TESTS=1 (as CI's sudo unshare step
// sets), where missing privilege is a failure instead.
func RequireMountPrivilege(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Mount(dir, dir, "", unix.MS_BIND, ""); err != nil {
		if errors.Is(err, unix.EPERM) && os.Getenv("REQUIRE_MOUNT_TESTS") != "1" {
			t.Skip("requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	_ = unix.Unmount(dir, 0)
}

// WriteSquashfs writes a squashfs image at path holding one file,
// hello.txt = "hello from the image", padded to the 4 KiB blocks the kernel
// reads it in through a loop device.
func WriteSquashfs(t *testing.T, path string) {
	t.Helper()
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	w, err := squashfs.NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddFile("hello.txt", []byte("hello from the image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	fi, err := out.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Truncate((fi.Size() + 4095) / 4096 * 4096); err != nil {
		t.Fatal(err)
	}
}
