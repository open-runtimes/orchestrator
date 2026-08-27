package erofs

import (
	"fmt"
	"testing"

	"orchestrator/internal/erofs/disk"
)

func TestPlanLayoutAlignsSmallDirectoryToInline(t *testing.T) {
	root := &erofsEntry{name: "/", path: "/", mode: disk.StatTypeDir | 0o755, nlink: 2}
	// Make the root directory too large to inline, then fill metadata with
	// compact special-file inodes so the target starts at offset 4064 within
	// the third metadata block.
	for i := range 382 {
		root.children = append(root.children, &erofsEntry{
			name: fmt.Sprintf("a%03d", i),
			path: fmt.Sprintf("/a%03d", i),
			mode: disk.StatTypeChrdev | 0o600,
		})
	}
	target := &erofsEntry{name: "z", path: "/z", mode: disk.StatTypeDir | 0o755, nlink: 2}
	root.children = append(root.children, target)

	w := &erofsWriter{blockSize: 4096, compactInodes: true}
	w.planLayout(root)

	if target.layout != disk.LayoutFlatInline {
		t.Fatalf("target layout = %d, want inline", target.layout)
	}
	const wantNID = 3 * 4096 / 32
	if target.nid != wantNID {
		t.Fatalf("target nid = %d, want next metadata block nid %d", target.nid, wantNID)
	}
}
