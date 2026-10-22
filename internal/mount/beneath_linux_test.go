//go:build linux

package mount

import (
	"errors"
	"orchestrator/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLocal(t *testing.T) {
	for _, bad := range []string{"", ".", "/abs", "../up", "a/../../up"} {
		if _, err := Local(bad); err == nil {
			t.Errorf("Local(%q) accepted a path outside the workspace", bad)
		}
	}
	if got, err := Local("a/./b/"); err != nil || got != "a/b" {
		t.Fatalf("Local(a/./b/) = %q, %v", got, err)
	}
}

// The workspace is writable by the workload, so every entry the agent touches
// may have been replaced by a symlink pointing anywhere on the node. None of
// the Root operations may follow one, and none needs privilege to check.
func TestRootRefusesSymlinks(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("node file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dest := range map[string]string{"dirlink": outside, "filelink": filepath.Join(outside, "secret"), "dotdot": ".."} {
		if err := os.Symlink(dest, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	root, err := OpenRoot(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, rel := range []string{"dirlink", "dirlink/x", "dotdot"} {
		if f, err := root.OpenDir(rel); err == nil {
			f.Close()
			t.Errorf("OpenDir(%q) followed a symlink", rel)
		}
	}
	if f, err := root.OpenFile("filelink"); err == nil {
		f.Close()
		t.Error("OpenFile(filelink) followed a symlink")
	}
	if f, err := root.MkdirAll("dirlink/created", 0o755); err == nil {
		f.Close()
		t.Error("MkdirAll created a directory through a symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory appeared outside the root: %v", err)
	}

	// RemoveAll removes the link, never what it points at.
	if err := root.RemoveAll("dirlink"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatalf("RemoveAll reached through a symlink: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "dirlink")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink itself not removed: %v", err)
	}

	// And a real tree, including a link inside it, goes entirely.
	if f, err := root.MkdirAll("tree/a/b", 0o755); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	if err := os.Symlink(outside, filepath.Join(ws, "tree/a/escape")); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveAll("tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "tree")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tree not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatalf("removing a tree followed a link inside it: %v", err)
	}
}

func TestBeneathRefusesSymlinkedPaths(t *testing.T) {
	ws, outside := t.TempDir(), t.TempDir()
	testutil.WriteSquashfs(t, filepath.Join(ws, "img.sqfs"))
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "img.sqfs"), filepath.Join(ws, "img-link")); err != nil {
		t.Fatal(err)
	}
	b := Beneath{Dir: ws}

	if err := b.Mount("img.sqfs", "out/code", Opts{}); err == nil {
		t.Fatal("mounted through a symlinked target")
	}
	if err := b.Mount("img-link", "code", Opts{}); err == nil {
		t.Fatal("mounted a symlinked source")
	}
	if err := b.Mount("img.sqfs", ".", Opts{}); err == nil {
		t.Fatal("mounted over the workspace root")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("something was created outside the root: %v", entries)
	}
}

// Needs CAP_SYS_ADMIN in a private mount namespace: CI runs it under sudo
// unshare; unprivileged runs skip.
func TestBeneathMountLifecycle(t *testing.T) {
	testutil.RequireMountPrivilege(t)
	ws := t.TempDir()
	// Model the workspace volume: a mount point of its own.
	if err := unix.Mount(ws, ws, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(ws, unix.MNT_DETACH) })
	testutil.WriteSquashfs(t, filepath.Join(ws, "img.sqfs"))
	b := Beneath{Dir: ws}

	t.Run("read-only image", func(t *testing.T) {
		if err := b.Mount("img.sqfs", "ro", Opts{}); err != nil {
			t.Fatal(err)
		}
		assertContent(t, filepath.Join(ws, "ro/hello.txt"), "hello from the image")
		assertMounted(t, b, "ro", true)
		if err := os.WriteFile(filepath.Join(ws, "ro/new"), nil, 0o644); err == nil {
			t.Fatal("read-only image accepted a write")
		}
		if err := b.Unmount("ro"); err != nil {
			t.Fatal(err)
		}
		assertMounted(t, b, "ro", false)
		assertNoMountsUnder(t, filepath.Join(ws, "ro"))
	})

	t.Run("writable overlay", func(t *testing.T) {
		if err := b.Mount("img.sqfs", "rw", Opts{Writable: true, SizeMiB: 8}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "rw/new.txt"), []byte("delta"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertContent(t, filepath.Join(ws, UpperDir("rw"), "new.txt"), "delta")
		assertContent(t, filepath.Join(ws, "rw/hello.txt"), "hello from the image")
		if err := b.Unmount("rw"); err != nil {
			t.Fatal(err)
		}
		assertMounted(t, b, "rw", false)
		assertNoMountsUnder(t, filepath.Join(ws, "rw"))
		for _, gone := range []string{ScratchDir("rw"), LowerDir("rw")} {
			if _, err := os.Stat(filepath.Join(ws, gone)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s left behind: %v", gone, err)
			}
		}
	})

	t.Run("synced overlay keeps its upper on disk", func(t *testing.T) {
		if err := b.Mount("img.sqfs", "synced", Opts{Writable: true, UpperOnDisk: true}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, "synced/new.txt"), []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
		// An on-disk upper is an ordinary directory in the workspace, which is
		// what lets the runner archive the delta while the overlay is up.
		var st unix.Statfs_t
		if err := unix.Statfs(filepath.Join(ws, ScratchDir("synced")), &st); err != nil {
			t.Fatal(err)
		}
		if st.Type == unix.TMPFS_MAGIC {
			t.Fatal("synced upper landed on tmpfs")
		}
		if err := b.Unmount("synced"); err != nil {
			t.Fatal(err)
		}
		assertNoMountsUnder(t, filepath.Join(ws, "synced"))
	})

	t.Run("directory source", func(t *testing.T) {
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
		assertContent(t, filepath.Join(ws, "tree/data"), "extracted")
		assertMounted(t, b, "tree", true)
		if err := os.WriteFile(filepath.Join(ws, "tree/new"), nil, 0o644); err == nil {
			t.Fatal("read-only directory mount accepted a write")
		}
		if err := b.Unmount("tree"); err != nil {
			t.Fatal(err)
		}
		assertNoMountsUnder(t, filepath.Join(ws, "tree"))
	})

	// The target can itself be a Kubernetes volume mount (the runtime pod nests
	// its code volume inside the workspace), and the workload can hold a file
	// open in the mount when it is released. Unmount must take only the
	// artifact mounts, detach the busy one rather than fail, and stay safe to
	// repeat.
	t.Run("busy reader is detached and a volume under the target survives", func(t *testing.T) {
		nested := filepath.Join(ws, "nested")
		if err := os.MkdirAll(filepath.Join(nested, ".lower"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(nested, nested, "", unix.MS_BIND, ""); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Unmount(nested, unix.MNT_DETACH) })
		if err := os.WriteFile(filepath.Join(nested, ".lower/data"), []byte("still readable"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.Mount(LowerDir("nested"), "nested", Opts{Writable: true, SourceDir: true, SizeMiB: 16}); err != nil {
			t.Fatal(err)
		}
		reader, err := os.Open(filepath.Join(nested, "data"))
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if err := b.Unmount("nested"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 32)
		if n, err := reader.Read(buf); err != nil || string(buf[:n]) != "still readable" {
			t.Fatalf("detached reader: %q, %v", buf[:n], err)
		}
		if err := b.Unmount("nested"); err != nil {
			t.Fatalf("repeat cleanup: %v", err)
		}
		info, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			t.Fatal(err)
		}
		if got := MountsUnder(string(info), nested); len(got) != 1 || got[0] != nested {
			t.Fatalf("mounts under the target = %v, want only the volume", got)
		}
	})

	t.Run("failed mount leaves nothing behind", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(ws, "junk.img"), []byte("not an image"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := b.Mount("junk.img", "broken", Opts{Writable: true}); err == nil {
			t.Fatal("mounted a file that is not an image")
		}
		assertNoMountsUnder(t, filepath.Join(ws, "broken"))
		assertNoMountsUnder(t, filepath.Join(ws, ScratchDir("broken")))
	})
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertMounted(t *testing.T, b Beneath, rel string, want bool) {
	t.Helper()
	got, err := b.IsMounted(rel)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("IsMounted(%s) = %v, want %v", rel, got, want)
	}
}

func assertNoMountsUnder(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if left := MountsUnder(string(data), dir); len(left) > 0 {
		t.Fatalf("mounts left under %s: %s", dir, strings.Join(left, ", "))
	}
}
