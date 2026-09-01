package sidecar

import (
	"path/filepath"
	"strconv"
	"strings"
)

// hasArtifactMount scans mountinfo for the newest mount entry at target and
// reports whether it looks like a mount this sidecar establishes.
//
// mountinfo fields: id parent major:minor root mountpoint options [optional
// fields...] - fstype source superopts. Later lines stack over earlier ones,
// so the last entry for the path is the visible mount.
func hasArtifactMount(mountinfo, target string) bool {
	target = filepath.Clean(target)
	var root, fstype string
	found := false
	for line := range strings.SplitSeq(mountinfo, "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		postFields := strings.Fields(post)
		if len(fields) < 5 || len(postFields) < 1 {
			continue
		}
		if unescapeMountPath(fields[4]) != target {
			continue
		}
		found = true
		root = unescapeMountPath(fields[3])
		fstype = postFields[0]
	}
	if !found {
		return false
	}
	switch fstype {
	case "overlay", "squashfs", "erofs":
		return true
	}
	// A read-only tar mount is a bind of the extracted .lower tree, so its
	// fstype is the volume's own; the bind root gives it away.
	return strings.Contains(root, "/.lower")
}

// unescapeMountPath reverses the octal escaping mountinfo applies to spaces,
// tabs, newlines, and backslashes in paths.
func unescapeMountPath(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
