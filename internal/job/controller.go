package job

import (
	"context"
	"fmt"
	"orchestrator/internal/apperrors"
	"sync"
	"time"
)

// Viewer is the read-only surface used by HTTP handlers.
type Viewer interface {
	Get(jobID string) (Entry, bool)
	List() []Entry
}

// Controller is the full lifecycle surface for an orchestrator backend.
// T is the runtime handle type (e.g. dockerHandle in the docker package).
//
// Lifecycle: Reserve → Commit → [Notifier.Notify via watcher] → Release
// Reconcile: Restore → [Notifier.Notify via watcher] → Release
type Controller[T any] interface {
	Viewer
	Reserve(jobID string) error
	Commit(jobID string, runtime T, cancelWatch context.CancelFunc) Notifier
	Restore(jobID string, t Transition, runtime T, cancelWatch context.CancelFunc) (Notifier, error)
	Release(jobID string) (Handle[T], bool)
	Each(f func(string, Entry, Handle[T]))
}

// Notifier is the pre-bound write handle given to a watcher goroutine.
// It does not expose the full Controller; it can only drive FSM transitions
// for the single job it was bound to at Commit or Restore time.
type Notifier interface {
	Notify(t Transition) error
}

// controllerEntry is the internal record in StoreController.
type controllerEntry[T any] struct {
	jobEntry    Entry
	handle      T
	cancelWatch context.CancelFunc
	released    bool // set by Release; Notify returns an error if true
}

// notifier is the Notifier implementation returned by Commit and Restore.
// It holds a pointer to the controller's mutex and to its own entry so that
// Notify can drive FSM transitions without a map lookup.
type notifier[T any] struct {
	mu    *sync.RWMutex
	e     *controllerEntry[T]
	jobID string
}

func (n *notifier[T]) Notify(t Transition) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.e.released {
		return fmt.Errorf("job %s: job has been released", n.jobID)
	}

	if err := ValidateTransition(n.e.jobEntry.State, t.State()); err != nil {
		return fmt.Errorf("job %s: %w", n.jobID, err)
	}

	n.e.jobEntry.State = t.State()
	n.e.jobEntry.UpdatedAt = time.Now()
	if code := t.ExitCode(); code != nil {
		c := *code
		n.e.jobEntry.ExitCode = &c
	}
	if msg := t.ErrMsg(); msg != "" {
		n.e.jobEntry.Error = msg
	}
	return nil
}

// StoreController implements Controller[T].
// It is the single source of truth for job lifecycle state and runtime handles.
type StoreController[T any] struct {
	mu   sync.RWMutex
	jobs map[string]*controllerEntry[T]
}

// NewStoreController creates a new StoreController.
func NewStoreController[T any]() *StoreController[T] {
	return &StoreController[T]{jobs: make(map[string]*controllerEntry[T])}
}

// Reserve atomically claims a job ID and seeds it at StateAccepted.
// Returns a conflict error if the ID is already taken.
func (c *StoreController[T]) Reserve(jobID string) error {
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

// Commit stores the runtime handle and returns a Notifier pre-bound to this job.
// If the job does not exist, a dead Notifier is returned (always errors on Notify).
func (c *StoreController[T]) Commit(jobID string, runtime T, cancelWatch context.CancelFunc) Notifier {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.jobs[jobID]
	if e == nil {
		return &deadNotifier{jobID: jobID}
	}
	e.handle = runtime
	e.cancelWatch = cancelWatch
	return &notifier[T]{mu: &c.mu, e: e, jobID: jobID}
}

// Restore seeds the controller with a job recovered at startup.
// Unlike Reserve+Commit, the initial state is taken directly from t.
// Pass nil cancelWatch for terminal jobs.
func (c *StoreController[T]) Restore(jobID string, t Transition, runtime T, cancelWatch context.CancelFunc) (Notifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.jobs[jobID]; exists {
		return nil, fmt.Errorf("job %s already registered", jobID)
	}

	now := time.Now()
	e := &controllerEntry[T]{
		jobEntry: Entry{
			ID:        jobID,
			State:     t.State(),
			CreatedAt: now,
			UpdatedAt: now,
		},
		handle:      runtime,
		cancelWatch: cancelWatch,
	}
	if code := t.ExitCode(); code != nil {
		cp := *code
		e.jobEntry.ExitCode = &cp
	}
	if msg := t.ErrMsg(); msg != "" {
		e.jobEntry.Error = msg
	}
	c.jobs[jobID] = e
	return &notifier[T]{mu: &c.mu, e: e, jobID: jobID}, nil
}

// Release atomically removes a job and returns its handle for cleanup.
// Returns (zero, false) if the job does not exist.
func (c *StoreController[T]) Release(jobID string) (Handle[T], bool) {
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
func (c *StoreController[T]) Get(jobID string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, exists := c.jobs[jobID]
	if !exists {
		return Entry{}, false
	}
	return e.jobEntry, true
}

// List returns snapshots of all entries.
func (c *StoreController[T]) List() []Entry {
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
func (c *StoreController[T]) Each(f func(string, Entry, Handle[T])) {
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

// deadNotifier is returned by Commit when Reserve was not called first.
type deadNotifier struct{ jobID string }

func (d *deadNotifier) Notify(_ Transition) error {
	return fmt.Errorf("job %s: notifier is dead (Reserve was not called)", d.jobID)
}

// Compile-time interface checks.
var (
	_ Controller[struct{}] = (*StoreController[struct{}])(nil)
	_ Notifier             = (*notifier[struct{}])(nil)
	_ Notifier             = (*deadNotifier)(nil)
)
