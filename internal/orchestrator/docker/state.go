package docker

import (
	"context"
	"orchestrator/internal/apperrors"
	"sync"
)

// jobState holds the runtime state for a single job.
type jobState struct {
	jobContainerID     string
	sidecarContainerID string
	volumeName         string
	cancelWatch        context.CancelFunc
}

// stateRepo manages job state with thread-safe access.
type stateRepo struct {
	mu   sync.RWMutex
	jobs map[string]*jobState
}

// newStateRepo creates a new state repository.
func newStateRepo() *stateRepo {
	return &stateRepo{
		jobs: make(map[string]*jobState),
	}
}

// reserve attempts to reserve a job ID slot. Returns error if already exists.
// The slot is reserved with nil until commit is called.
func (r *stateRepo) reserve(jobID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.jobs[jobID]; exists {
		return apperrors.Conflict("job", jobID, "job already exists")
	}
	r.jobs[jobID] = nil
	return nil
}

// commit fills in a reserved slot with the actual job state.
func (r *stateRepo) commit(jobID string, js *jobState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[jobID] = js
}

// release removes a job from the repository. Returns the state if it existed.
func (r *stateRepo) release(jobID string) (*jobState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	js, exists := r.jobs[jobID]
	if exists {
		delete(r.jobs, jobID)
	}
	return js, exists
}

// get retrieves a job's state. Returns (nil, true) if reserved but not yet committed.
func (r *stateRepo) get(jobID string) (*jobState, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	js, exists := r.jobs[jobID]
	return js, exists
}

// list returns all job IDs and their states.
func (r *stateRepo) list() map[string]*jobState {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]*jobState, len(r.jobs))
	for id, js := range r.jobs {
		result[id] = js
	}
	return result
}

// ids returns all job IDs.
func (r *stateRepo) ids() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.jobs))
	for id := range r.jobs {
		ids = append(ids, id)
	}
	return ids
}
