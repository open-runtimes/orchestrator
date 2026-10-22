package mount

import (
	"path/filepath"
	"strconv"
	"strings"
)

// HasArtifactMount scans mountinfo for the newest mount entry at target and
// reports whether it looks like an artifact mount (one a Mounter establishes).
//
// mountinfo fields: id parent major:minor root mountpoint options [optional
// fields...] - fstype source superopts. Later lines stack over earlier ones,
// so the last entry for the path is the visible mount.
func HasArtifactMount(mountinfo, target string) bool {
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
		if UnescapeMountPath(fields[4]) != target {
			continue
		}
		found = true
		root = UnescapeMountPath(fields[3])
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

// MountsUnder lists every mount point at or below dir, in mountinfo order (so
// parents before the mounts stacked inside them). Unmounting in reverse order
// takes a tree apart innermost first.
func MountsUnder(mountinfo, dir string) []string {
	dir = filepath.Clean(dir)
	var out []string
	for line := range strings.SplitSeq(mountinfo, "\n") {
		pre, _, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(pre)
		if len(fields) < 5 {
			continue
		}
		p := UnescapeMountPath(fields[4])
		if p == dir || strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	return out
}

// UnescapeMountPath reverses the octal escaping mountinfo applies to spaces,
// tabs, newlines, and backslashes in paths.
func UnescapeMountPath(s string) string {
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
