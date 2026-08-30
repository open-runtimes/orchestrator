package artifact

import (
	"context"
	"errors"
)

// Sync bounds. A floor on the interval keeps a mount from turning into a
// request storm against the object store; the default is what a caller gets by
// asking for sync and nothing else, and it is the amount of work at risk if the
// pod dies without a flush.
const (
	MinSyncIntervalSeconds     = 5
	DefaultSyncIntervalSeconds = 60
)

// Mount mounts a squashfs or erofs image into the workspace so the worker can
// read it without extraction. Tar archives (plain, gzip, zstd, or lz4) are
// accepted as a compatibility path: they are extracted into a private lower
// directory and bind-mounted at the target. The format is detected from magic.
// By default the mount is read-only; set Writable to layer a tmpfs-backed
// overlay on top, giving the worker a copy-on-write view whose writes are
// discarded when the job ends. Size caps that tmpfs (in MiB); 0 leaves it at
// the kernel default (half of RAM), bounded by the pod's memory.
//
// Unlike other artifacts, a mount is not applied-and-done: the mount must exist
// before the worker starts and persist for its whole lifetime, then be torn
// down afterwards. That lifecycle is owned by the sidecar runner, which detects
// Mount artifacts and drives the host Mounter directly, so Apply is never
// reached through the normal artifact flow.
//
// A workload with no post-phase sidecar therefore cannot have a mount at all —
// which is why MountDef declares NeedsPostPhase and the serving registry rejects
// it (internal/artifact/registry.go). Jobs are the only plane that runs one.
type Mount struct {
	ID       string `json:"id"`
	In       string `json:"in"`  // Source image or tar archive path in the workspace
	Out      string `json:"out"` // Mount point directory (in the workspace)
	Writable bool   `json:"writable,omitempty"`
	Size     int    `json:"size,omitempty"` // Overlay tmpfs cap in MiB (writable only; 0 = default)
	// Sync is where this overlay's delta is kept — the changes made on top of
	// the image, and nothing else. The workload restores from it when the mount
	// is established, pushes to it on an interval, and flushes to it on the way
	// out, so a workspace survives its sandbox without the orchestrator owning
	// any storage. Requires Writable: without an overlay there is no delta.
	//
	// Setting it also moves the upper layer off tmpfs and onto the shared
	// workspace, so Size no longer applies — the workspace volume's own limit
	// does.
	Sync string `json:"sync,omitempty"`
	// SyncIntervalSeconds is how often the delta is pushed while the workload
	// runs; 0 takes the default. The flush on the way out happens regardless, so
	// this is how much work is at risk if the pod dies without one.
	SyncIntervalSeconds int    `json:"syncIntervalSeconds,omitempty"`
	Depends             string `json:"depends,omitempty"`
}

func (a *Mount) ArtifactID() string   { return a.ID }
func (a *Mount) ArtifactType() string { return "mount" }
func (a *Mount) DependsOn() string    { return a.Depends }

// HasMount reports whether any artifact is a mount. Orchestrators use
// this to decide whether a job needs the privileged, propagation-enabled pod.
func HasMount(artifacts []Artifact) bool {
	for _, a := range artifacts {
		if a.ArtifactType() == "mount" {
			return true
		}
	}
	return false
}

// Apply must never run: the sidecar establishes mounts out of band (see the
// type doc). Reaching here means a mount slipped into the ordinary artifact
// flow, so it fails loudly rather than reporting a success nobody performed.
func (a *Mount) Apply(context.Context, string) *Result {
	return &Result{Status: "error", Error: errors.New("mount artifacts are established by the sidecar, not applied")}
}
