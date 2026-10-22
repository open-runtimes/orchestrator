//go:build linux

package node

import (
	"context"
	"crypto/rand"
	"net/http/httptest"
	"orchestrator/internal/mount"
	"orchestrator/internal/mountagent"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
)

// A request is only ever served in the workspace its token names, and a
// token the agent never published opens nothing.
func TestAPIRefusesUnknownTokens(t *testing.T) {
	a, err := New("test", "node", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	for _, token := range []string{"", "short", rand.Text()} {
		_, err := mountagent.NewClient(srv.URL, token).IsMounted("code")
		if err == nil || !strings.Contains(err.Error(), "unknown workspace token") {
			t.Errorf("token %q: IsMounted error = %v, want an authorization refusal", token, err)
		}
	}
}

func TestPublishRefusesOtherNamespaces(t *testing.T) {
	a, err := New("test", "node", t.TempDir(), []string{"jobs"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.NodePublishVolume(context.Background(), publishRequest(t.TempDir(), "intruder", rand.Text()))
	if err == nil || !strings.Contains(err.Error(), "may not use") {
		t.Fatalf("publish from another namespace: %v, want a refusal", err)
	}
}

// End to end through the seams production crosses: kubelet's publish, the
// sidecar's client over HTTP, the kernel, and kubelet's unpublish.
func TestPublishMountUnpublish(t *testing.T) {
	testutil.RequireMountPrivilege(t)
	a, err := New("test", "node", t.TempDir(), []string{"ns"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// kubelet's target for an inline volume, inside a pod directory.
	target := filepath.Join(t.TempDir(), "pods/uid/volumes/kubernetes.io~csi/workspace/mount")
	token := rand.Text()
	ctx := context.Background()
	publish := publishRequest(target, "ns", token)
	if _, err := a.NodePublishVolume(ctx, publish); err != nil {
		t.Fatal(err)
	}
	// A retried publish is a no-op, not a second bind.
	if _, err := a.NodePublishVolume(ctx, publish); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	if n := len(mountsUnder(t, target)); n != 1 {
		t.Fatalf("publish left %d mounts at the target, want 1", n)
	}
	var st unix.Statfs_t
	if err := unix.Statfs(target, &st); err != nil {
		t.Fatal(err)
	}
	if st.Flags&unix.ST_NODEV == 0 || st.Flags&unix.ST_NOSUID == 0 {
		t.Errorf("workspace mount flags = %#x, want nodev and nosuid", st.Flags)
	}

	// A second pod cannot claim this workspace's token.
	if _, err := a.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId: "csi-other", TargetPath: filepath.Join(t.TempDir(), "other"), VolumeContext: publish.VolumeContext,
	}); err == nil {
		t.Fatal("a second volume was bound to a token already in use")
	}

	c := mountagent.NewClient(srv.URL, token)
	testutil.WriteSquashfs(t, filepath.Join(target, "code.sqfs"))
	if err := c.Mount("code.sqfs", "code", mount.Opts{Writable: true, SizeMiB: 8}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "code/hello.txt")); err != nil || string(got) != "hello from the image" {
		t.Fatalf("mounted content = %q, %v", got, err)
	}
	if mounted, err := c.IsMounted("code"); err != nil || !mounted {
		t.Fatalf("IsMounted = %v, %v", mounted, err)
	}

	// What the workload can plant where an image should be is refused before
	// anything is opened for reading: a FIFO would hang the agent, a device
	// node would be opened on the host.
	if err := unix.Mkfifo(filepath.Join(target, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Mount("fifo", "f", mount.Opts{}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("mounting a FIFO: %v, want a refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mounting a FIFO hung the agent")
	}

	// Paths outside the workspace are refused.
	for _, bad := range []string{"../escape", "/etc", "."} {
		if err := c.Mount("code.sqfs", bad, mount.Opts{}); err == nil {
			t.Errorf("agent mounted at %q", bad)
		}
	}

	// Leave the overlay up: unpublish must take it apart on its own, as it
	// does when a sidecar dies before releasing.
	if _, err := a.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: "csi-test", TargetPath: target}); err != nil {
		t.Fatal(err)
	}
	if left := mountsUnder(t, target); len(left) > 0 {
		t.Fatalf("mounts left after unpublish: %v", left)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target left after unpublish: %v", err)
	}
	if _, err := c.IsMounted("code"); err == nil {
		t.Fatal("token still works after unpublish")
	}
}

func publishRequest(target, namespace, token string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:   "csi-test",
		TargetPath: target,
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/ephemeral":     "true",
			"csi.storage.k8s.io/pod.namespace": namespace,
			"csi.storage.k8s.io/pod.name":      "job-x",
			mountagent.AttributeToken:          token,
		},
	}
}

func mountsUnder(t *testing.T, dir string) []string {
	t.Helper()
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	return mount.MountsUnder(string(info), dir)
}
