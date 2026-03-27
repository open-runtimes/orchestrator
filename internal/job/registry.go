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

// TransitionForExit returns ToCompleted for exit code 0, ToFailed otherwise.
// This is the canonical mapping from a container exit code to a job terminal state.
func TransitionForExit(exitCode int) Transition {
	if exitCode == 0 {
		return ToCompleted(exitCode)
	}
	return ToFailed(exitCode, "")
}

// Handle pairs a cancel function with a runtime-specific handle T.
// Returned by Release so the caller can stop the watcher and clean up resources.
type Handle[T any] struct {
	CancelWatch context.CancelFunc
	Runtime     T
}
