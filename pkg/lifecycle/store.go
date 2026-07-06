package lifecycle

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
	Get(id string) (Entry, bool)
	List() []Entry
}

// Store is the full lifecycle surface for an orchestrator backend.
// T is the runtime handle type (e.g. dockerHandle in the docker package).
//
// Lifecycle: Reserve → Commit → [Apply via watcher] → Release
// Reconcile: Reserve → Commit → Apply (to replay known state) → [Apply via watcher] → Release
type Store[T any] interface {
	Viewer
	Reserve(id string) error
	Commit(id string, runtime T, cancelWatch context.CancelFunc)
	Apply(id string, s Signal) error
	Release(id string) (Handle[T], bool)
	Each(f func(string, Entry, Handle[T]))
}

// storeEntry is the internal record in MemoryStore.
type storeEntry[T any] struct {
	entry       Entry
	handle      T
	cancelWatch context.CancelFunc
	released    bool // set by Release; Apply returns an error if true
}

// MemoryStore implements Store[T].
// It is the single source of truth for lifecycle state and runtime handles.
type MemoryStore[T any] struct {
	kind string // resource kind used in error messages (e.g. "job")

	mu      sync.RWMutex
	entries map[string]*storeEntry[T]
}

// NewMemoryStore creates a new MemoryStore. kind names the resource in
// errors (e.g. "job").
func NewMemoryStore[T any](kind string) *MemoryStore[T] {
	return &MemoryStore[T]{kind: kind, entries: make(map[string]*storeEntry[T])}
}

// Reserve atomically claims an ID and seeds it at StateAccepted.
// Returns a conflict error if the ID is already taken.
func (c *MemoryStore[T]) Reserve(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[id]; exists {
		return apperrors.Conflict(c.kind, id, c.kind+" already exists")
	}

	now := time.Now()
	c.entries[id] = &storeEntry[T]{
		entry: Entry{
			ID:        id,
			State:     StateAccepted,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
	return nil
}

// Commit stores the runtime handle for a reserved entry.
func (c *MemoryStore[T]) Commit(id string, runtime T, cancelWatch context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e := c.entries[id]; e != nil {
		e.handle = runtime
		e.cancelWatch = cancelWatch
	}
}

// Apply translates a Signal into an FSM state change for the given entry.
// LogLine signals are ignored (no state change). Returns an error if the entry
// is not found, already released, or the signal results in an invalid transition.
func (c *MemoryStore[T]) Apply(id string, s Signal) error {
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

	e, ok := c.entries[id]
	if !ok || e.released {
		return fmt.Errorf("%s %s: not found or released", c.kind, id)
	}

	if err := validateTransition(e.entry.State, targetState); err != nil {
		return fmt.Errorf("%s %s: %w", c.kind, id, err)
	}

	e.entry.State = targetState
	e.entry.UpdatedAt = time.Now()
	if exitCode != nil {
		cp := *exitCode
		e.entry.ExitCode = &cp
	}
	if errMsg != "" {
		e.entry.Error = errMsg
	}
	return nil
}

// Release atomically removes an entry and returns its handle for cleanup.
// Returns (zero, false) if the entry does not exist.
func (c *MemoryStore[T]) Release(id string) (Handle[T], bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, exists := c.entries[id]
	if !exists {
		return Handle[T]{}, false
	}
	e.released = true
	delete(c.entries, id)
	return Handle[T]{CancelWatch: e.cancelWatch, Runtime: e.handle}, true
}

// Get returns a snapshot of the entry for the given ID.
func (c *MemoryStore[T]) Get(id string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, exists := c.entries[id]
	if !exists {
		return Entry{}, false
	}
	return e.entry, true
}

// List returns snapshots of all entries.
func (c *MemoryStore[T]) List() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Entry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.entry)
	}
	return out
}

// Each calls f for every entry. A snapshot is taken before the walk so
// Release calls inside f are safe.
func (c *MemoryStore[T]) Each(f func(string, Entry, Handle[T])) {
	c.mu.RLock()
	type snap struct {
		id string
		e  Entry
		h  Handle[T]
	}
	snaps := make([]snap, 0, len(c.entries))
	for id, e := range c.entries {
		snaps = append(snaps, snap{
			id: id,
			e:  e.entry,
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
