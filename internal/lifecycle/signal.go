// Package lifecycle provides the backend-agnostic state machine for
// run-to-completion workloads: the sealed Signal set a watcher emits, the Entry
// snapshot with its FSM, the rules that name a state (StateForExit), and the
// MemoryStore for backends that hold state in memory.
//
// Its consumer is the jobs service, through pkg/job. Serving workloads —
// deployments and sandboxes do not run to completion and have
// their own vocabulary; they derive status from the backend rather than from a
// store here. Keep this package free of anything shaped like either one: what
// belongs here is what a workload does between starting and exiting.
package lifecycle

import "time"

// Signal is the sealed set of backend-agnostic signals a lifecycle watcher
// emits during a workload's execution. Backends translate their native
// signals (Docker events, Kubernetes pod phases, etc.) into these types
// before sending.
type Signal interface {
	signal()
}

// Started is emitted when the workload container has started successfully.
type Started struct{}

// Exited is emitted when the workload container exits.
type Exited struct {
	ExitCode int
	Duration time.Duration

	// Reason names why the workload terminated when the backend can attest
	// to a cause beyond the exit code (e.g. ExitReasonOOM). Empty when the
	// backend has nothing to add — consumers must treat unknown values as
	// equivalent to empty.
	Reason string
}

// ExitReasonOOM marks a workload killed by the kernel OOM killer.
const ExitReasonOOM = "oom"

// Failed is emitted when the workload fails before or without starting
// (e.g. sidecar crash, failure to start the container).
type Failed struct {
	Reason string
}

// Completed is emitted when all work for the workload has finished, including
// post-exit processing (e.g. post-job artifacts). Always follows Exited.
type Completed struct{}

// LogLine is emitted for each batch of stdout/stderr lines from the workload.
type LogLine struct {
	Stream string // "stdout" or "stderr"
	Lines  []string
}

func (Started) signal()   {}
func (Exited) signal()    {}
func (Failed) signal()    {}
func (Completed) signal() {}
func (LogLine) signal()   {}
