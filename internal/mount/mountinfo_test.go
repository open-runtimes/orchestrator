package mount

import (
	"strings"
	"testing"
)

func TestHasArtifactMount(t *testing.T) {
	// The runtime pod nests the code volume inside the workspace volume, so
	// the bare target is already a mount point — the case that must NOT read
	// as an artifact mount.
	bareNestedVolume := `1128 1113 254:1 /var/lib/kubelet/pods/x/volumes/kubernetes.io~empty-dir/source /workspace rw,relatime - ext4 /dev/vda1 rw
1129 1128 254:1 /var/lib/kubelet/pods/x/volumes/kubernetes.io~empty-dir/code /workspace/code rw,relatime - ext4 /dev/vda1 rw`

	overlayStacked := bareNestedVolume + `
1204 1129 0:105 / /workspace/code rw,relatime - overlay overlayfs rw,lowerdir=/workspace/code/.lower,upperdir=/ws/.scratch/upper`

	imageMounted := bareNestedVolume + `
1204 1129 7:3 / /workspace/code ro,nodev,relatime - squashfs /dev/loop3 ro`

	erofsMounted := bareNestedVolume + `
1204 1129 7:4 / /workspace/code ro,nodev,relatime - erofs /dev/loop4 ro`

	tarBind := bareNestedVolume + `
1204 1129 254:1 /var/lib/kubelet/pods/x/volumes/kubernetes.io~empty-dir/code/.lower /workspace/code ro,relatime - ext4 /dev/vda1 rw`

	// Docker combined flow: the workspace is one volume and the target is a
	// plain directory with no entry at all until an artifact mount lands.
	dockerBare := `900 800 254:1 /var/lib/docker/volumes/ws/_data /workspace rw - ext4 /dev/vda1 rw`

	for _, tc := range []struct {
		name      string
		mountinfo string
		want      bool
	}{
		{"bare nested volume is not an artifact mount", bareNestedVolume, false},
		{"stacked overlay is", overlayStacked, true},
		{"squashfs image is", imageMounted, true},
		{"erofs image is", erofsMounted, true},
		{"tar bind of the .lower tree is", tarBind, true},
		{"docker bare directory is not", dockerBare, false},
		{"missing path is not", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasArtifactMount(tc.mountinfo, "/workspace/code"); got != tc.want {
				t.Fatalf("HasArtifactMount() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnescapeMountPath(t *testing.T) {
	if got := UnescapeMountPath(`/mnt/with\040space`); got != "/mnt/with space" {
		t.Fatalf("UnescapeMountPath() = %q", got)
	}
	if got := UnescapeMountPath("/plain/path"); got != "/plain/path" {
		t.Fatalf("UnescapeMountPath() = %q", got)
	}
}

func TestMountsUnder(t *testing.T) {
	info := `20 1 254:1 / / rw - ext4 /dev/vda1 rw
30 20 0:40 / /var/lib/kubelet/pods/p/volumes/csi/ws rw shared:5 - ext4 /dev/vda1 rw
31 30 7:3 / /var/lib/kubelet/pods/p/volumes/csi/ws/code/.lower ro - erofs /dev/loop3 ro
32 30 0:41 / /var/lib/kubelet/pods/p/volumes/csi/ws/code.scratch rw - tmpfs none rw
33 30 0:42 / /var/lib/kubelet/pods/p/volumes/csi/ws/code rw - overlay overlayfs rw
34 20 0:43 / /var/lib/kubelet/pods/p/volumes/csi/wsother rw - tmpfs none rw`
	got := MountsUnder(info, "/var/lib/kubelet/pods/p/volumes/csi/ws/")
	want := []string{
		"/var/lib/kubelet/pods/p/volumes/csi/ws",
		"/var/lib/kubelet/pods/p/volumes/csi/ws/code/.lower",
		"/var/lib/kubelet/pods/p/volumes/csi/ws/code.scratch",
		"/var/lib/kubelet/pods/p/volumes/csi/ws/code",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("MountsUnder() = %q, want %q", got, want)
	}
}
