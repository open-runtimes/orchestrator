package sidecar

import (
	"orchestrator/internal/artifact"
	"orchestrator/internal/mount"
	"orchestrator/internal/mountagent"
	"path/filepath"
	"strings"
)

// Mounter mounts a source at a target directory and unmounts it, both given
// relative to the workspace. The source is normally a read-only filesystem
// image (squashfs or erofs); with SourceDir it is a directory materialized from
// a tar archive. With opts.Writable the source becomes the read-only lower
// layer of an overlay. mount.Beneath mounts from this container;
// mountagent.Client has the node mount agent do it.
type Mounter interface {
	Mount(source, target string, opts mount.Opts) error
	Unmount(target string) error
	IsMounted(target string) (bool, error)
}

// fetcher is a Mounter that can also fetch an image through the node's cache
// (mountagent.Client).
type fetcher interface {
	FetchMount(url, target string, opts mount.Opts) (mountagent.FetchMountResponse, error)
}

// cachedDownloads pairs each mount with the download that feeds it, when the
// node agent can fetch that download through its cache instead: the mount reads
// the download's file and nothing else does, the download needs no headers of
// its own, and neither waits on another artifact. The pre phase skips those
// downloads, and the mount phase hands each pair to the agent as one
// fetch-mount. With any other Mounter there are none.
func (r *Runner) cachedDownloads(arts []artifact.Artifact) map[*artifact.Mount]*artifact.Download {
	if _, ok := r.mounter.(fetcher); !ok {
		return nil
	}
	readers := map[string]int{} // file -> artifacts that read it or wait on its download
	for _, a := range arts {
		if src := artifact.SourceFile(a); src != "" {
			readers[filepath.Clean(src)]++
		}
	}
	downloads := map[string]*artifact.Download{} // by output file
	for _, a := range arts {
		if d, ok := a.(*artifact.Download); ok {
			downloads[filepath.Clean(d.Out)] = d
		}
	}
	for _, a := range arts {
		if dep := a.DependsOn(); dep != "" {
			for out, d := range downloads {
				if d.ID == dep {
					readers[out]++
				}
			}
		}
	}

	pairs := map[*artifact.Mount]*artifact.Download{}
	for _, a := range arts {
		m, ok := a.(*artifact.Mount)
		if !ok || m.Sync != "" {
			continue
		}
		in := filepath.Clean(m.In)
		d := downloads[in]
		if d == nil || d.Depends != "" || len(d.Headers) > 0 || (m.Depends != "" && m.Depends != d.ID) {
			continue
		}
		// The mount itself counts once as a reader, and once more if it
		// waits on the download.
		want := 1
		if m.Depends == d.ID {
			want = 2
		}
		scheme, _, _ := strings.Cut(d.In, "://")
		if readers[in] == want && (scheme == "https" || scheme == "http" || scheme == "s3") {
			pairs[m] = d
		}
	}
	return pairs
}

// withoutCached drops the downloads the mount phase fetches through the cache.
func withoutCached(arts []artifact.Artifact, pairs map[*artifact.Mount]*artifact.Download) []artifact.Artifact {
	if len(pairs) == 0 {
		return arts
	}
	skip := map[artifact.Artifact]bool{}
	for _, d := range pairs {
		skip[d] = true
	}
	var kept []artifact.Artifact
	for _, a := range arts {
		if !skip[a] {
			kept = append(kept, a)
		}
	}
	return kept
}

// splitMounts separates mount artifacts from the rest, preserving order.
func splitMounts(arts []artifact.Artifact) (mounts []*artifact.Mount, rest []artifact.Artifact) {
	for _, a := range arts {
		if m, ok := a.(*artifact.Mount); ok {
			mounts = append(mounts, m)
		} else {
			rest = append(rest, a)
		}
	}
	return mounts, rest
}
