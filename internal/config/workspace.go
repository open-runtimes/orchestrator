package config

// The shared workspace is the one directory every process of a workload agrees
// on: the backend mounts a volume there, the sidecar materializes artifacts
// into it, the shim opens its exec FIFO in it, and the worker runs in it. The
// path travels from the backend to those processes in EnvSharedVolume.
//
// Name, default and reader live here because a mismatch is not a compile error
// in any of them — it is a container that starts and cannot find its command.
const (
	// EnvSharedVolume carries the workspace path to every process in a workload.
	EnvSharedVolume = "SHARED_VOLUME_PATH"
	// DefaultWorkspace is the path used when nothing states another: the API's
	// default for a request, and what each process falls back to.
	DefaultWorkspace = "/workspace"
)

// Workspace returns the shared workspace path this process was given, or the
// default when it was given none.
func Workspace() string { return GetEnv(EnvSharedVolume, DefaultWorkspace) }
