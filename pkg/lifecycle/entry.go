package lifecycle

import (
	"fmt"
	"time"
)

// State constants for the run-to-completion FSM.
const (
	StateAccepted  = "accepted"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

// Entry represents a workload's state snapshot.
type Entry struct {
	ID        string
	State     string
	ExitCode  *int
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
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
