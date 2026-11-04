//go:build linux

package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// IDRange maps Length ids from ContainerID inside a user namespace onto
// HostID onwards outside it, as one line of /proc/<pid>/uid_map does.
type IDRange struct {
	ContainerID uint32
	HostID      uint32
	Length      uint32
}

// IDMap is the user namespace a workload runs in (Kubernetes `hostUsers:
// false`). The mounts made for it are ID-mapped into that namespace
// (MOUNT_ATTR_IDMAP), so files the image stores as root belong to the
// workload's root instead of to an id the namespace cannot map, which it sees
// as 65534 and may not write. A writable overlay additionally needs its upper
// and work directories owned by the host ids the namespace's root maps to:
// overlayfs itself cannot be ID-mapped, and an ID-mapped upper goes read-only.
type IDMap struct {
	userns *os.File
	// UID and GID are the host ids of the namespace's root.
	UID, GID uint32
}

// NewIDMap builds a user namespace with the given mappings, held by
// descriptor. The kernel only takes a namespace for MOUNT_ATTR_IDMAP from a
// process, so a child is cloned into one, the mappings are written, and the
// child is killed once the namespace is open. It is traced, so it stops at
// exec and runs nothing of the binary.
func NewIDMap(uids, gids []IDRange) (*IDMap, error) {
	m := &IDMap{}
	var found bool
	if m.UID, found = rootOf(uids); !found {
		return nil, errors.New("the uid mapping does not map root")
	}
	if m.GID, found = rootOf(gids); !found {
		return nil, errors.New("the gid mapping does not map root")
	}

	// A traced child is traced by the thread that started it.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// Stopped at exec and killed below, so there is nothing for a context to cancel.
	cmd := exec.CommandContext(context.Background(), "/proc/self/exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWUSER,
		UidMappings: sysIDMap(uids),
		GidMappings: sysIDMap(gids),
		Ptrace:      true,
		Pdeathsig:   syscall.SIGKILL,
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("create user namespace: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	ns, err := os.Open(fmt.Sprintf("/proc/%d/ns/user", cmd.Process.Pid))
	if err != nil {
		return nil, fmt.Errorf("open user namespace: %w", err)
	}
	m.userns = ns
	return m, nil
}

// Close releases the namespace.
func (m *IDMap) Close() error { return m.userns.Close() }

// attr adds the mapping to a mount_setattr request.
func (m *IDMap) attr(a *unix.MountAttr) {
	if m != nil {
		a.Attr_set |= unix.MOUNT_ATTR_IDMAP
		a.Userns_fd = uint64(m.userns.Fd())
	}
}

// chown gives a directory to the namespace's root.
func (m *IDMap) chown(dir *os.File) error {
	if m == nil {
		return nil
	}
	return unix.Fchown(int(dir.Fd()), int(m.UID), int(m.GID))
}

func rootOf(ranges []IDRange) (uint32, bool) {
	for _, r := range ranges {
		if r.ContainerID == 0 && r.Length > 0 {
			return r.HostID, true
		}
	}
	return 0, false
}

func sysIDMap(ranges []IDRange) []syscall.SysProcIDMap {
	out := make([]syscall.SysProcIDMap, len(ranges))
	for i, r := range ranges {
		out[i] = syscall.SysProcIDMap{ContainerID: int(r.ContainerID), HostID: int(r.HostID), Size: int(r.Length)}
	}
	return out
}
