package docker

import (
	"context"
	"fmt"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/job"
	"sync"
	"time"
)

// dockerHandle carries the Docker infrastructure identifiers for a running job.
type dockerHandle struct {
	sidecarContainerID string
	jobContainerID     string
	volumeName         string
}

// registryEntry is the internal record combining FSM state with the Docker handle.
type registryEntry struct {
	jobEntry    job.Entry
	handle      dockerHandle
	cancelWatch context.CancelFunc
}

// dockerRegistry implements job.Registry[dockerHandle].
// It is the single source of truth for job state and Docker infrastructure handles.
type dockerRegistry struct {
	mu   sync.RWMutex
	jobs map[string]*registryEntry
}

func newDockerRegistry() *dockerRegistry {
	return &dockerRegistry{jobs: make(map[string]*registryEntry)}
}

// Reserve atomically claims a job ID and sets the initial state to Accepted.
func (r *dockerRegistry) Reserve(jobID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.jobs[jobID]; exists {
		return apperrors.Conflict("job", jobID, "job already exists")
	}

	now := time.Now()
	r.jobs[jobID] = &registryEntry{
		jobEntry: job.Entry{
			ID:        jobID,
			State:     job.StateAccepted,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
	return nil
}

// Commit stores the runtime handle and cancel function after containers are created.
// Must only be called after a successful Reserve.
func (r *dockerRegistry) Commit(jobID string, runtime dockerHandle, cancelWatch context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if entry, exists := r.jobs[jobID]; exists {
		entry.handle = runtime
		entry.cancelWatch = cancelWatch
	}
}

// Restore seeds the registry with a job recovered from Docker at startup.
// This is initialization, not a transition — there is no prior state to validate against.
// Pass nil cancelWatch for terminal jobs.
func (r *dockerRegistry) Restore(jobID string, t job.Transition, runtime dockerHandle, cancelWatch context.CancelFunc) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.jobs[jobID]; exists {
		return fmt.Errorf("job %s already registered", jobID)
	}

	now := time.Now()
	entry := &registryEntry{
		jobEntry: job.Entry{
			ID:        jobID,
			State:     t.State(),
			CreatedAt: now,
			UpdatedAt: now,
		},
		handle:      runtime,
		cancelWatch: cancelWatch,
	}
	if code := t.ExitCode(); code != nil {
		c := *code
		entry.jobEntry.ExitCode = &c
	}
	if msg := t.ErrMsg(); msg != "" {
		entry.jobEntry.Error = msg
	}

	r.jobs[jobID] = entry
	return nil
}

// Apply drives an FSM-validated state transition.
func (r *dockerRegistry) Apply(jobID string, t job.Transition) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, exists := r.jobs[jobID]
	if !exists {
		return fmt.Errorf("job %s not found", jobID)
	}

	if err := job.ValidateTransition(entry.jobEntry.State, t.State()); err != nil {
		return err
	}

	entry.jobEntry.State = t.State()
	entry.jobEntry.UpdatedAt = time.Now()
	if code := t.ExitCode(); code != nil {
		c := *code
		entry.jobEntry.ExitCode = &c
	}
	if msg := t.ErrMsg(); msg != "" {
		entry.jobEntry.Error = msg
	}

	return nil
}

// Release atomically removes a job and returns its handle for cleanup.
func (r *dockerRegistry) Release(jobID string) (job.Handle[dockerHandle], bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, exists := r.jobs[jobID]
	if !exists {
		return job.Handle[dockerHandle]{}, false
	}

	delete(r.jobs, jobID)
	return job.Handle[dockerHandle]{
		CancelWatch: entry.cancelWatch,
		Runtime:     entry.handle,
	}, true
}

// Get returns a snapshot of the job entry.
func (r *dockerRegistry) Get(jobID string) (job.Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, exists := r.jobs[jobID]
	if !exists {
		return job.Entry{}, false
	}
	return entry.jobEntry, true
}

// List returns snapshots of all entries.
func (r *dockerRegistry) List() []job.Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]job.Entry, 0, len(r.jobs))
	for _, e := range r.jobs {
		entries = append(entries, e.jobEntry)
	}
	return entries
}

// Each calls f for every job. A snapshot is taken before the walk.
func (r *dockerRegistry) Each(f func(jobID string, e job.Entry, h job.Handle[dockerHandle])) {
	r.mu.RLock()
	type snap struct {
		id    string
		entry job.Entry
		h     job.Handle[dockerHandle]
	}
	snaps := make([]snap, 0, len(r.jobs))
	for id, e := range r.jobs {
		snaps = append(snaps, snap{
			id:    id,
			entry: e.jobEntry,
			h: job.Handle[dockerHandle]{
				CancelWatch: e.cancelWatch,
				Runtime:     e.handle,
			},
		})
	}
	r.mu.RUnlock()

	for _, s := range snaps {
		f(s.id, s.entry, s.h)
	}
}

// Verify dockerRegistry implements job.Registry[dockerHandle].
var _ job.Registry[dockerHandle] = (*dockerRegistry)(nil)
