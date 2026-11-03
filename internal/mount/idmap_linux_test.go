//go:build linux

package mount

import (
	"orchestrator/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// podRoot is the host uid and gid a user-namespaced pod's root maps to in
// these tests, the way kubelet allocates one 65536-id range per pod.
const podRoot = 196608

var podIDs = []IDRange{{ContainerID: 0, HostID: podRoot, Length: 65536}}

// A `hostUsers: false` pod can read and write its mounts as its own root: the
// image's root-owned files are its root's, and the writable overlay takes
// writes, edits, deletes and new directories inside image directories. Run
// from the pod's side, in a user namespace with the pod's mapping, because
// what host root can do proves nothing. Needs the same privilege as
// TestBeneathMountLifecycle.
func TestBeneathIDMapped(t *testing.T) {
	testutil.RequireMountPrivilege(t)
	ws := t.TempDir()
	// The pod's root must be able to reach the workspace, as it can a volume.
	for _, d := range []string{filepath.Dir(ws), ws} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mount(ws, ws, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(ws, unix.MNT_DETACH) })
	testutil.WriteErofs(t, filepath.Join(ws, "code.erofs"))
	idmap, err := NewIDMap(podIDs, podIDs)
	if err != nil {
		t.Fatal(err)
	}
	defer idmap.Close()
	b := Beneath{Dir: ws, IDMap: idmap}

	t.Run("read-only image belongs to the pod's root", func(t *testing.T) {
		if err := b.Mount("code.erofs", "ro", Opts{}); err != nil {
			t.Fatal(err)
		}
		defer b.Unmount("ro")
		assertOwner(t, filepath.Join(ws, "ro/hello.txt"), podRoot)
		inPod(t, "test \"$(stat -c %u ro/hello.txt)\" = 0 && cat ro/hello.txt >/dev/null", ws)
	})

	t.Run("writable overlay takes the pod's writes", func(t *testing.T) {
		if err := b.Mount("code.erofs", "rw", Opts{Writable: true, SizeMiB: 8}); err != nil {
			t.Fatal(err)
		}
		defer b.Unmount("rw")
		inPod(t, `set -e
cd rw
test "$(stat -c %u .)" = 0
test "$(stat -c %u hello.txt)" = 0
echo new > new.txt
echo more >> hello.txt
rm dir/data
mkdir dir/sub`, ws)
		assertOwner(t, filepath.Join(ws, "rw/new.txt"), podRoot)
		assertContent(t, filepath.Join(ws, "rw/hello.txt"), "hello from the imagemore\n")
	})

	t.Run("directory source belongs to the pod's root", func(t *testing.T) {
		lower := filepath.Join(ws, LowerDir("tree"))
		if err := os.MkdirAll(lower, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lower, "data"), []byte("extracted"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.Mount(LowerDir("tree"), "tree", Opts{SourceDir: true}); err != nil {
			t.Fatal(err)
		}
		defer b.Unmount("tree")
		assertOwner(t, filepath.Join(ws, "tree/data"), podRoot)
	})

	t.Run("synced mount is refused", func(t *testing.T) {
		if err := b.Mount("code.erofs", "synced", Opts{Writable: true, UpperOnDisk: true}); err == nil {
			t.Fatal("mounted a synced overlay in a user namespace")
		}
		assertNoMountsUnder(t, filepath.Join(ws, "synced"))
	})
}

// inPod runs script in dir as root of a new user namespace with the pod's
// mapping.
func inPod(t *testing.T, script, dir string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
	cmd.Dir = dir
	ids := []syscall.SysProcIDMap{{ContainerID: 0, HostID: podRoot, Size: 65536}}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER, UidMappings: ids, GidMappings: ids, Credential: &syscall.Credential{}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("in the pod's user namespace: %v\n%s", err, out)
	}
}

func assertOwner(t *testing.T, path string, uid uint32) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != uid || st.Gid != uid {
		t.Fatalf("%s is owned by %d:%d, want %d:%d", path, st.Uid, st.Gid, uid, uid)
	}
}
