package job

import "context"

// Transition describes a state change to apply to a job.
// Construct using the package-level helpers; never build the struct directly.
type Transition struct {
	state    string
	exitCode *int
	errMsg   string
}

// State returns the target state for the transition.
func (t Transition) State() string { return t.state }

// ExitCode returns the exit code, or nil if not set.
func (t Transition) ExitCode() *int { return t.exitCode }

// ErrMsg returns the error message, or empty string if not set.
func (t Transition) ErrMsg() string { return t.errMsg }

// ToAccepted is used during reconciliation for jobs that have not yet started.
func ToAccepted() Transition { return Transition{state: StateAccepted} }

// ToRunning is emitted when the sidecar is healthy and the worker has started.
func ToRunning() Transition { return Transition{state: StateRunning} }

// ToCompleted is emitted when the worker exits with code 0.
func ToCompleted(exitCode int) Transition {
	return Transition{state: StateCompleted, exitCode: &exitCode}
}

// ToFailed is emitted when the worker exits non-zero, or the sidecar exits early.
func ToFailed(exitCode int, reason string) Transition {
	return Transition{state: StateFailed, exitCode: &exitCode, errMsg: reason}
}

// ToCancelled is used by Stop.
func ToCancelled() Transition { return Transition{state: StateCancelled} }

// Handle pairs a cancel function with a runtime-specific handle T.
// Returned by Release so the caller can stop the watcher and clean up resources.
type Handle[T any] struct {
	CancelWatch context.CancelFunc
	Runtime     T
}

// Registry is the unified state store for job lifecycle management.
// T is the runtime handle type (e.g. dockerHandle in the docker package).
//
// Lifecycle: Reserve → Commit → Apply (via watcher) → Release.
type Registry[T any] interface {
	// Reserve atomically claims a job ID and sets the initial state to Accepted.
	// Returns an error if the ID is already taken.
	Reserve(jobID string) error

	// Commit stores the runtime handle and cancel function after backend resources
	// are created. Must only be called after a successful Reserve.
	Commit(jobID string, runtime T, cancelWatch context.CancelFunc)

	// Apply drives an FSM-validated state transition. This is the only method
	// the watcher loop calls. Returns an error if the transition is invalid.
	Apply(jobID string, t Transition) error

	// Release atomically removes a job and returns its handle for cleanup.
	// Returns (zero, false) if the job does not exist.
	Release(jobID string) (Handle[T], bool)

	// Get returns a snapshot of the job entry. Used by the HTTP Status handler.
	Get(jobID string) (Entry, bool)

	// List returns snapshots of all entries. Used by the HTTP List handler.
	List() []Entry

	// Each calls f for every job. A snapshot is taken before the walk so
	// Release calls inside f are safe.
	Each(f func(jobID string, e Entry, h Handle[T]))
}
