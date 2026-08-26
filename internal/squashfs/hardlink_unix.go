//go:build unix

package squashfs

import "syscall"

// getDevIno extracts the device and inode numbers from a FileInfo.Sys() value.
func getDevIno(sys any) (devIno, bool) {
	if sys == nil {
		return devIno{}, false
	}
	st, ok := sys.(*syscall.Stat_t)
	if !ok {
		return devIno{}, false
	}
	// The conversions are load-bearing on some platforms: syscall.Stat_t
	// field widths differ per GOOS (Dev is int32 on darwin, uint64 on linux).
	return devIno{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true //nolint:unconvert
}
