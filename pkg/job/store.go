package job

import "orchestrator/pkg/lifecycle"

// The lifecycle state machine (Entry, Signal, MemoryStore and friends) lives
// in pkg/lifecycle so it can be shared with the deployments/pools serving
// plane. These aliases keep pkg/job the vocabulary for job backends.
type (
	Entry              = lifecycle.Entry
	Signal             = lifecycle.Signal
	Started            = lifecycle.Started
	Exited             = lifecycle.Exited
	Failed             = lifecycle.Failed
	LogLine            = lifecycle.LogLine
	Handle[T any]      = lifecycle.Handle[T]
	Viewer             = lifecycle.Viewer
	Store[T any]       = lifecycle.Store[T]
	MemoryStore[T any] = lifecycle.MemoryStore[T]
)

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

// Compile-time check that the alias wiring stays intact.
var _ Store[struct{}] = (*MemoryStore[struct{}])(nil)
