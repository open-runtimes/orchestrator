package job

import (
	"context"
	"fmt"
	"orchestrator/internal/apperrors"
	"sync"
	"time"
)

// Handle pairs a cancel function with a runtime-specific handle T.
// Returned by Release so the caller can stop the watcher and clean up resources.
type Handle[T any] struct {
	CancelWatch context.CancelFunc
	Runtime     T
}

// Viewer is the read-only surface used by HTTP handlers.
type Viewer interface {
	Get(jobID string) (Entry, bool)
	List() []Entry
}

// Store is the full lifecycle surface for an orchestrator backend.
// T is the runtime handle type (e.g. dockerHandle in the docker package).
//
// Lifecycle: Reserve → Commit → [Apply via watcher] → Release
// Reconcile: Reserve → Commit → Apply (to replay known state) → [Apply via watcher] → Release
type Store[T any] interface {
	Viewer
	Reserve(jobID string) error
	Commit(jobID string, runtime T, cancelWatch context.CancelFunc)
	Apply(jobID string, s Signal) error
	Release(jobID string) (Handle[T], bool)
	Each(f func(string, Entry, Handle[T]))
}

// controllerEntry is the internal record in MemoryStore.
type controllerEntry[T any] struct {
	jobEntry    Entry
	handle      T
	cancelWatch context.CancelFunc
	released    bool // set by Release; Apply returns an error if true
}

// MemoryStore implements Store[T].
// It is the single source of truth for job lifecycle state and runtime handles.
type MemoryStore[T any] struct {
	mu   sync.RWMutex
	jobs map[string]*controllerEntry[T]
}

// NewMemoryStore creates a new MemoryStore.
func NewMemoryStore[T any]() *MemoryStore[T] {
	return &MemoryStore[T]{jobs: make(map[string]*controllerEntry[T])}
}

// Reserve atomically claims a job ID and seeds it at StateAccepted.
// Returns a conflict error if the ID is already taken.
func (c *MemoryStore[T]) Reserve(jobID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.jobs[jobID]; exists {
		return apperrors.Conflict("job", jobID, "job already exists")
	}

	now := time.Now()
	c.jobs[jobID] = &controllerEntry[T]{
		jobEntry: Entry{
			ID:        jobID,
			State:     StateAccepted,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
	return nil
}

// Commit stores the runtime handle for a reserved job.
func (c *MemoryStore[T]) Commit(jobID string, runtime T, cancelWatch context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e := c.jobs[jobID]; e != nil {
		e.handle = runtime
		e.cancelWatch = cancelWatch
	}
}

// Apply translates a Signal into an FSM state change for the given job.
// LogLine signals are ignored (no state change). Returns an error if the job
// is not found, already released, or the signal results in an invalid transition.
func (c *MemoryStore[T]) Apply(jobID string, s Signal) error {
	var targetState string
	var exitCode *int
	var errMsg string

	switch ev := s.(type) {
	case Started:
		targetState = StateRunning
	case Exited:
		code := ev.ExitCode
		exitCode = &code
		if ev.ExitCode == 0 {
			targetState = StateCompleted
		} else {
			targetState = StateFailed
		}
	case Failed:
		targetState = StateFailed
		code := -1
		exitCode = &code
		errMsg = ev.Reason
	default:
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.jobs[jobID]
	if !ok || e.released {
		return fmt.Errorf("job %s: not found or released", jobID)
	}

	if err := validateTransition(e.jobEntry.State, targetState); err != nil {
		return fmt.Errorf("job %s: %w", jobID, err)
	}

	e.jobEntry.State = targetState
	e.jobEntry.UpdatedAt = time.Now()
	if exitCode != nil {
		cp := *exitCode
		e.jobEntry.ExitCode = &cp
	}
	if errMsg != "" {
		e.jobEntry.Error = errMsg
	}
	return nil
}

// Release atomically removes a job and returns its handle for cleanup.
// Returns (zero, false) if the job does not exist.
func (c *MemoryStore[T]) Release(jobID string) (Handle[T], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, exists := c.jobs[jobID]
	if !exists {
		return Handle[T]{}, false
	}
	e.released = true
	delete(c.jobs, jobID)
	return Handle[T]{CancelWatch: e.cancelWatch, Runtime: e.handle}, true
}

// Get returns a snapshot of the entry for the given job ID.
func (c *MemoryStore[T]) Get(jobID string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, exists := c.jobs[jobID]
	if !exists {
		return Entry{}, false
	}
	return e.jobEntry, true
}

// List returns snapshots of all entries.
func (c *MemoryStore[T]) List() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Entry, 0, len(c.jobs))
	for _, e := range c.jobs {
		out = append(out, e.jobEntry)
	}
	return out
}

// Each calls f for every job. A snapshot is taken before the walk so
// Release calls inside f are safe.
func (c *MemoryStore[T]) Each(f func(string, Entry, Handle[T])) {
	c.mu.RLock()
	type snap struct {
		id string
		e  Entry
		h  Handle[T]
	}
	snaps := make([]snap, 0, len(c.jobs))
	for id, e := range c.jobs {
		snaps = append(snaps, snap{
			id: id,
			e:  e.jobEntry,
			h:  Handle[T]{CancelWatch: e.cancelWatch, Runtime: e.handle},
		})
	}
	c.mu.RUnlock()

	for i := range snaps {
		f(snaps[i].id, snaps[i].e, snaps[i].h)
	}
}

// Compile-time interface check.
var _ Store[struct{}] = (*MemoryStore[struct{}])(nil)
