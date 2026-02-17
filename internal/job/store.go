package job

import (
	"fmt"
	"sync"
	"time"
)

// Entry represents a job's state in the store.
type Entry struct {
	ID        string
	State     string
	ExitCode  *int
	Error     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Status converts an Entry to a job.Status for API responses.
func (e *Entry) Status() *Status {
	s := &Status{
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

// Option configures optional fields when setting job state.
type Option func(*Entry)

// WithExitCode sets the exit code on the entry.
func WithExitCode(code int) Option {
	return func(e *Entry) {
		e.ExitCode = &code
	}
}

// WithError sets the error message on the entry.
func WithError(msg string) Option {
	return func(e *Entry) {
		e.Error = msg
	}
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

// Store is a thread-safe, in-memory store for job state with FSM-enforced transitions.
type Store struct {
	mu   sync.RWMutex
	jobs map[string]*Entry
}

// NewStore creates a new Store.
func NewStore() *Store {
	return &Store{
		jobs: make(map[string]*Entry),
	}
}

// Set creates or transitions a job's state.
// If the job doesn't exist, a new entry is created in the given state.
// If the job exists, the transition is validated against the FSM.
func (s *Store) Set(jobID, state string, opts ...Option) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	existing, exists := s.jobs[jobID]
	if !exists {
		entry := &Entry{
			ID:        jobID,
			State:     state,
			CreatedAt: now,
			UpdatedAt: now,
		}
		for _, opt := range opts {
			opt(entry)
		}
		s.jobs[jobID] = entry
		return nil
	}

	// Validate FSM transition
	allowed, ok := validTransitions[existing.State]
	if !ok || !allowed[state] {
		return fmt.Errorf("invalid state transition: %s -> %s", existing.State, state)
	}

	existing.State = state
	existing.UpdatedAt = now
	for _, opt := range opts {
		opt(existing)
	}
	return nil
}

// Get returns a copy of the entry for the given job ID.
func (s *Store) Get(jobID string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, exists := s.jobs[jobID]
	if !exists {
		return Entry{}, false
	}
	return *entry, true
}

// List returns copies of all entries.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entries := make([]Entry, 0, len(s.jobs))
	for _, entry := range s.jobs {
		entries = append(entries, *entry)
	}
	return entries
}

// Remove deletes an entry from the store.
func (s *Store) Remove(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, exists := s.jobs[jobID]
	if exists {
		delete(s.jobs, jobID)
	}
	return exists
}
