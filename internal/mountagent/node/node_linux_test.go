//go:build linux

package node

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/mount"
	"orchestrator/internal/mountagent"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// A pod kubelet runs with `hostUsers: false` gets its mounts ID-mapped into
// its user namespace, from the mapping kubelet recorded for it; a pod without
// that record gets them unmapped, as before.
func TestPublishMountInUserNamespace(t *testing.T) {
	testutil.RequireMountPrivilege(t)
	a, err := New("test", "node", t.TempDir(), []string{"r-*"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	ctx := context.Background()
	for _, tc := range []struct {
		uid    string
		userns string
		owner  uint32
	}{
		{"pod-userns", `{"uidMappings":[{"hostId":196608,"containerId":0,"length":65536}],"gidMappings":[{"hostId":196608,"containerId":0,"length":65536}]}`, 196608},
		{"pod-host", "", 0},
	} {
		t.Run(tc.uid, func(t *testing.T) {
			podDir := filepath.Join(t.TempDir(), "pods", tc.uid)
			target := filepath.Join(podDir, "volumes/kubernetes.io~csi/workspace/mount")
			if tc.userns != "" {
				if err := os.MkdirAll(podDir, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(podDir, "userns"), []byte(tc.userns), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			token := rand.Text()
			publish := publishRequest(target, "r-"+tc.uid, token)
			publish.VolumeId = "csi-" + tc.uid
			if _, err := a.NodePublishVolume(ctx, publish); err != nil {
				t.Fatal(err)
			}
			defer a.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: publish.VolumeId, TargetPath: target})

			testutil.WriteErofs(t, filepath.Join(target, "code.erofs"))
			if err := mountagent.NewClient(srv.URL, token).Mount("code.erofs", "code", mount.Opts{Writable: true, SizeMiB: 8}); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{"code", "code/hello.txt"} {
				var st unix.Stat_t
				if err := unix.Stat(filepath.Join(target, p), &st); err != nil {
					t.Fatal(err)
				}
				if st.Uid != tc.owner || st.Gid != tc.owner {
					t.Errorf("%s is owned by %d:%d, want %d", p, st.Uid, st.Gid, tc.owner)
				}
			}
		})
	}
}

func TestNamespacePrefixes(t *testing.T) {
	a := &Agent{namespaces: []string{"jobs", "r-*", "*"}}
	for ns, want := range map[string]bool{"jobs": true, "r-abc": true, "r-": true, "jobs-2": false, "default": false} {
		if got := a.allowed(ns); got != want {
			t.Errorf("allowed(%q) = %v, want %v", ns, got, want)
		}
	}
}

// A pod's image comes from the node's cache: the first pod downloads it, the
// next one mounts the same file, and an object the kernel would not mount is
// refused before anything is stacked, so the sidecar can fetch it itself.
func TestPublishMountFromCache(t *testing.T) {
	testutil.RequireMountPrivilege(t)
	images := t.TempDir()
	testutil.WriteSquashfs(t, filepath.Join(images, "a.sqfs"))
	if err := os.WriteFile(filepath.Join(images, "build.tar"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	files := http.FileServer(http.Dir(images))
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sig") != "ok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Header.Get("Range") == "" {
			downloads.Add(1)
		}
		w.Header().Set("ETag", `"v1"`)
		files.ServeHTTP(w, r)
	}))
	defer store.Close()

	state := t.TempDir()
	a, err := New("test", "node", state, []string{"ns"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Cache, err = NewCache(filepath.Join(state, "cache"), []string{store.URL}, 1<<30, 1<<30); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	ctx := context.Background()
	for i, wantHit := range []bool{false, true} {
		target := filepath.Join(t.TempDir(), "workspace")
		token := rand.Text()
		publish := publishRequest(target, "ns", token)
		publish.VolumeId = fmt.Sprintf("csi-%d", i)
		if _, err := a.NodePublishVolume(ctx, publish); err != nil {
			t.Fatal(err)
		}
		c := mountagent.NewClient(srv.URL, token)

		res, err := c.FetchMount(store.URL+"/a.sqfs?sig=ok", "code", mount.Opts{Writable: true, SizeMiB: 8})
		if err != nil {
			t.Fatal(err)
		}
		if res.Hit != wantHit || res.Format != "squashfs" {
			t.Fatalf("pod %d: fetch-mount = %+v, want hit %v of a squashfs image", i, res, wantHit)
		}
		if got, err := os.ReadFile(filepath.Join(target, "code/hello.txt")); err != nil || string(got) != "hello from the image" {
			t.Fatalf("pod %d: mounted content = %q, %v", i, got, err)
		}
		// The overlay is the pod's own: a write lands in its upper, not in the
		// cached image every pod mounts.
		if err := os.WriteFile(filepath.Join(target, "code/hello.txt"), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}

		for _, bad := range []string{store.URL + "/a.sqfs?sig=forged", store.URL + "/build.tar?sig=ok"} {
			if _, err := c.FetchMount(bad, "bad", mount.Opts{}); err == nil {
				t.Fatalf("pod %d: mounted %s", i, bad)
			}
			if mounted, _ := c.IsMounted("bad"); mounted {
				t.Fatalf("pod %d: a refused fetch left a mount", i)
			}
		}
		if _, err := a.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: publish.VolumeId, TargetPath: target}); err != nil {
			t.Fatal(err)
		}
		if left := mountsUnder(t, target); len(left) > 0 {
			t.Fatalf("pod %d: mounts left after unpublish: %v", i, left)
		}
	}
	if n := downloads.Load(); n != 2 { // the image once, and the tar once
		t.Fatalf("downloaded %d times, want the image once and the tar once", n)
	}

	// An agent without a cache has no fetch-mount, so the sidecar downloads.
	plain, err := New("test", "node", t.TempDir(), []string{"ns"})
	if err != nil {
		t.Fatal(err)
	}
	plainSrv := httptest.NewServer(plain.Handler())
	defer plainSrv.Close()
	target := filepath.Join(t.TempDir(), "workspace")
	token := rand.Text()
	if _, err := plain.NodePublishVolume(ctx, publishRequest(target, "ns", token)); err != nil {
		t.Fatal(err)
	}
	if _, err := mountagent.NewClient(plainSrv.URL, token).FetchMount(store.URL+"/a.sqfs?sig=ok", "code", mount.Opts{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("fetch-mount without a cache: %v, want 404", err)
	}
	if _, err := plain.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: "csi-test", TargetPath: target}); err != nil {
		t.Fatal(err)
	}
}
