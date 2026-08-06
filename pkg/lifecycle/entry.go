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

// StateForExit names the state a workload is in once its worker has exited.
// The exit code is the whole rule: a clean exit completed, anything else
// failed.
//
// Every path that reports a workload's state answers with this — the in-memory
// store applying an Exited signal, and the backends that derive state from the
// cluster instead. That is the point: an API read and a callback describing the
// same moment must not disagree, and two backends must not disagree either.
func StateForExit(code int) string {
	if code == 0 {
		return StateCompleted
	}
	return StateFailed
}

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
