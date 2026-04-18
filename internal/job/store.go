package job

import (
	"fmt"
	"time"
)

// Entry represents a job's state snapshot.
type Entry struct {
	ID        string
	State     string
	ExitCode  *int
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// StatusResponse converts an Entry to a job.StatusResponse for API responses.
func (e *Entry) StatusResponse() *StatusResponse {
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

// validateTransition returns an error if the from→to transition is not allowed by the FSM.
func validateTransition(from, to string) error {
	allowed, ok := validTransitions[from]
	if !ok || !allowed[to] {
		return fmt.Errorf("invalid state transition: %s -> %s", from, to)
	}
	return nil
}

// validTransitions defines the FSM edges.
var validTransitions = map[string]map[string]bool{
	StateAccepted: {
		StateRunning:   true,
		StateFailed:    true,
		StateCancelled: true,
	},
	StateRunning: {
		StateCompleted: true,
		StateFailed:    true,
		StateCancelled: true,
	},
}
