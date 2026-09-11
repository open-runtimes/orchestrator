package job

import "orchestrator/internal/lifecycle"

// The run-to-completion state machine (Entry, Signal, MemoryStore and friends)
// lives in pkg/lifecycle, backend-agnostic and free of anything job-shaped.
// These aliases keep pkg/job the vocabulary a job backend reads.
//
// Both jobs backends share the vocabulary and the state rules; only the Docker
// one keeps a MemoryStore. Kubernetes derives state from the cluster instead,
// so a status read is correct on any replica whether or not it holds
// leadership — which is why StateForExit is a rule both can apply rather than
// state one of them owns — see lifecycle.StateForExit.
type (
	Entry              = lifecycle.Entry
	Signal             = lifecycle.Signal
	Started            = lifecycle.Started
	Exited             = lifecycle.Exited
	Failed             = lifecycle.Failed
	Completed          = lifecycle.Completed
	LogLine            = lifecycle.LogLine
	Handle[T any]      = lifecycle.Handle[T]
	MemoryStore[T any] = lifecycle.MemoryStore[T]
)

// ExitReasonOOM marks a workload killed by the kernel OOM killer.
const ExitReasonOOM = lifecycle.ExitReasonOOM

// NewMemoryStore creates a MemoryStore whose errors name the "job" resource.
func NewMemoryStore[T any]() *MemoryStore[T] {
	return lifecycle.NewMemoryStore[T]("job")
}

// StatusFromEntry converts an Entry to a job.StatusResponse for API responses.
func StatusFromEntry(e Entry) *StatusResponse {
	s := &StatusResponse{
		ID:    e.ID,
		State: e.State,
		Error: e.Error,
	}
	if e.ExitCode != nil {
		code := *e.ExitCode
		s.ExitCode = &code
	}
	return s
}
