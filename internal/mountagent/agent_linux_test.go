//go:build linux

package mountagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/mount"
	"orchestrator/internal/squashfs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
)

func newToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// A request is only ever served in the workspace its token names, and a
// token the agent never published opens nothing.
func TestAPIRefusesUnknownTokens(t *testing.T) {
	a, err := New(DefaultDriver, "test", "node", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	ws := t.TempDir()
	for _, token := range []string{"", "short", newToken(t)} {
		c := NewClient(srv.URL, token, ws)
		_, err := c.IsMounted(filepath.Join(ws, "code"))
		if err == nil || !strings.Contains(err.Error(), "unknown workspace token") {
			t.Errorf("token %q: IsMounted error = %v, want an authorization refusal", token, err)
		}
	}
}

// Records survive an agent restart, so a workspace kubelet published before
// the restart can still be mounted into and torn down after it.
func TestRegistryPersists(t *testing.T) {
	state := t.TempDir()
	reg, err := newRegistry(state)
	if err != nil {
		t.Fatal(err)
	}
	token := newToken(t)
	v := &Volume{ID: "csi-abc", Target: "/t", Data: "/d", TokenHash: hashToken(token), Pod: "ns/p"}
	if err := os.MkdirAll(reg.volumeDir(v.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := reg.put(v); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newRegistry(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.forToken(token); got == nil || got.Target != "/t" {
		t.Fatalf("reloaded registry lost the volume: %+v", got)
	}
	if err := reloaded.remove(v.ID); err != nil {
		t.Fatal(err)
	}
	if reloaded.forToken(token) != nil || reloaded.get(v.ID) != nil {
		t.Fatal("removed volume still resolves")
	}
	if _, err := os.Stat(reloaded.volumeDir(v.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("volume directory left behind: %v", err)
	}
}

// End to end through the seams production crosses: kubelet's publish, the
// sidecar's client over HTTP, the kernel, and kubelet's unpublish. Needs
// CAP_SYS_ADMIN in a private mount namespace (CI runs it under sudo unshare).
func TestPublishMountUnpublish(t *testing.T) {
	probe := t.TempDir()
	if err := unix.Mount(probe, probe, "", unix.MS_BIND, ""); err != nil {
		if errors.Is(err, unix.EPERM) && os.Getenv("REQUIRE_MOUNT_TESTS") != "1" {
			t.Skip("requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	_ = unix.Unmount(probe, 0)

	a, err := New(DefaultDriver, "test", "node", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	// kubelet's target for an inline volume, inside a pod directory.
	target := filepath.Join(t.TempDir(), "pods/uid/volumes/kubernetes.io~csi/workspace/mount")
	token := newToken(t)
	ctx := context.Background()
	publish := &csi.NodePublishVolumeRequest{
		VolumeId:   "csi-test",
		TargetPath: target,
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/ephemeral":     "true",
			"csi.storage.k8s.io/pod.namespace": "ns",
			"csi.storage.k8s.io/pod.name":      "job-x",
			AttributeToken:                     token,
		},
	}
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

	// The sidecar sees the workspace at its own path; here the target stands in.
	writeImage(t, filepath.Join(target, "code.sqfs"))
	c := NewClient(srv.URL, token, target)
	if err := c.Mount(filepath.Join(target, "code.sqfs"), filepath.Join(target, "code"), mount.Opts{Writable: true, SizeMiB: 8}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "code/hello.txt")); err != nil || string(got) != "hello from the image" {
		t.Fatalf("mounted content = %q, %v", got, err)
	}
	mounted, err := c.IsMounted(filepath.Join(target, "code"))
	if err != nil || !mounted {
		t.Fatalf("IsMounted = %v, %v", mounted, err)
	}
	// A path outside the workspace is refused by the client and the agent alike.
	if err := c.Mount(filepath.Join(target, "code.sqfs"), "/etc/evil", mount.Opts{}); err == nil {
		t.Fatal("client sent a target outside the workspace")
	}
	raw, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/mount", strings.NewReader(`{"source":"code.sqfs","target":"../escape"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(raw)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Fatal("agent mounted outside the workspace")
	}

	// Leave the overlay up: unpublish must take it apart on its own, as it
	// does when a sidecar dies before releasing.
	if _, err := a.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: "csi-test", TargetPath: target}); err != nil {
		t.Fatal(err)
	}
	if left := mountsUnder(t, target); len(left) > 0 {
		t.Fatalf("mounts left after unpublish: %v", left)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target left after unpublish: %v", err)
	}
	if _, err := c.IsMounted(filepath.Join(target, "code")); err == nil {
		t.Fatal("token still works after unpublish")
	}
}

func mountsUnder(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	return mount.MountsUnder(string(data), dir)
}

func writeImage(t *testing.T, path string) {
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
