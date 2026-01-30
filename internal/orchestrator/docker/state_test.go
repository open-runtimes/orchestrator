package docker

import (
	"sync"
	"testing"
)

func TestNewStateRepo(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()
	if repo == nil {
		t.Fatal("Expected non-nil repo")
	}
	if repo.jobs == nil {
		t.Fatal("Expected non-nil jobs map")
	}
	if len(repo.jobs) != 0 {
		t.Errorf("Expected empty jobs map, got %d entries", len(repo.jobs))
	}
}

func TestStateRepo_Reserve(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// First reserve should succeed
	err := repo.reserve("job-1")
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	// Job should exist with nil state
	js, exists := repo.get("job-1")
	if !exists {
		t.Error("Expected job to exist after reserve")
	}
	if js != nil {
		t.Error("Expected nil state for reserved job")
	}
}

func TestStateRepo_ReserveAlreadyExists(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Reserve first time
	if err := repo.reserve("job-1"); err != nil {
		t.Fatalf("First reserve failed: %v", err)
	}

	// Second reserve should fail
	err := repo.reserve("job-1")
	if err == nil {
		t.Error("Expected error for duplicate reserve")
	}
}

func TestStateRepo_ReserveAfterCommit(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Reserve and commit
	repo.reserve("job-1")
	repo.commit("job-1", &jobState{jobContainerID: "container-1"})

	// Reserve again should fail
	err := repo.reserve("job-1")
	if err == nil {
		t.Error("Expected error for reserve after commit")
	}
}

func TestStateRepo_Commit(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Reserve then commit
	repo.reserve("job-1")

	state := &jobState{
		jobContainerID:     "container-1",
		sidecarContainerID: "sidecar-1",
		volumeName:         "volume-1",
	}
	repo.commit("job-1", state)

	// Verify state
	js, exists := repo.get("job-1")
	if !exists {
		t.Fatal("Expected job to exist")
	}
	if js == nil {
		t.Fatal("Expected non-nil state after commit")
	}
	if js.jobContainerID != "container-1" {
		t.Errorf("Expected container-1, got %s", js.jobContainerID)
	}
	if js.sidecarContainerID != "sidecar-1" {
		t.Errorf("Expected sidecar-1, got %s", js.sidecarContainerID)
	}
}

func TestStateRepo_CommitWithoutReserve(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Commit without reserve should still work (creates the entry)
	state := &jobState{jobContainerID: "container-1"}
	repo.commit("job-1", state)

	js, exists := repo.get("job-1")
	if !exists {
		t.Error("Expected job to exist")
	}
	if js == nil || js.jobContainerID != "container-1" {
		t.Error("Expected committed state")
	}
}

func TestStateRepo_Release(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Setup: reserve and commit
	repo.reserve("job-1")
	state := &jobState{jobContainerID: "container-1"}
	repo.commit("job-1", state)

	// Release
	js, exists := repo.release("job-1")
	if !exists {
		t.Fatal("Expected exists=true for release")
	}
	if js == nil {
		t.Fatal("Expected non-nil state from release")
	}
	if js.jobContainerID != "container-1" {
		t.Errorf("Expected container-1, got %s", js.jobContainerID)
	}

	// Job should no longer exist
	_, exists = repo.get("job-1")
	if exists {
		t.Error("Expected job to not exist after release")
	}
}

func TestStateRepo_ReleaseNonExistent(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	js, exists := repo.release("nonexistent")
	if exists {
		t.Error("Expected exists=false for nonexistent job")
	}
	if js != nil {
		t.Error("Expected nil state for nonexistent job")
	}
}

func TestStateRepo_ReleaseReservedButNotCommitted(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Reserve but don't commit
	repo.reserve("job-1")

	// Release should return nil state but exists=true
	js, exists := repo.release("job-1")
	if !exists {
		t.Error("Expected exists=true for reserved job")
	}
	if js != nil {
		t.Error("Expected nil state for reserved but uncommitted job")
	}

	// Job should no longer exist
	_, exists = repo.get("job-1")
	if exists {
		t.Error("Expected job to not exist after release")
	}
}

func TestStateRepo_Get(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Nonexistent
	js, exists := repo.get("nonexistent")
	if exists {
		t.Error("Expected exists=false for nonexistent job")
	}
	if js != nil {
		t.Error("Expected nil state for nonexistent job")
	}

	// Reserved but not committed
	repo.reserve("job-1")
	js, exists = repo.get("job-1")
	if !exists {
		t.Error("Expected exists=true for reserved job")
	}
	if js != nil {
		t.Error("Expected nil state for reserved but uncommitted job")
	}

	// Committed
	state := &jobState{jobContainerID: "container-1"}
	repo.commit("job-1", state)
	js, exists = repo.get("job-1")
	if !exists {
		t.Error("Expected exists=true for committed job")
	}
	if js == nil || js.jobContainerID != "container-1" {
		t.Error("Expected committed state")
	}
}

func TestStateRepo_List(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Empty list
	jobs := repo.list()
	if len(jobs) != 0 {
		t.Errorf("Expected empty list, got %d entries", len(jobs))
	}

	// Add some jobs
	repo.reserve("job-1")
	repo.commit("job-1", &jobState{jobContainerID: "container-1"})
	repo.reserve("job-2")
	repo.commit("job-2", &jobState{jobContainerID: "container-2"})
	repo.reserve("job-3") // Reserved but not committed

	jobs = repo.list()
	if len(jobs) != 3 {
		t.Errorf("Expected 3 jobs, got %d", len(jobs))
	}

	// Verify it's a copy (modifying returned map shouldn't affect repo)
	delete(jobs, "job-1")
	if _, exists := repo.get("job-1"); !exists {
		t.Error("Deleting from list() result should not affect repo")
	}
}

func TestStateRepo_Ids(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Empty
	ids := repo.ids()
	if len(ids) != 0 {
		t.Errorf("Expected empty ids, got %d", len(ids))
	}

	// Add jobs
	repo.reserve("job-1")
	repo.commit("job-1", &jobState{})
	repo.reserve("job-2")
	repo.commit("job-2", &jobState{})

	ids = repo.ids()
	if len(ids) != 2 {
		t.Errorf("Expected 2 ids, got %d", len(ids))
	}

	// Check both IDs are present
	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}
	if !idSet["job-1"] || !idSet["job-2"] {
		t.Errorf("Expected job-1 and job-2, got %v", ids)
	}
}

func TestStateRepo_ConcurrentReserve(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Try to reserve the same job ID from multiple goroutines
	// Only one should succeed
	const numGoroutines = 100
	results := make(chan error, numGoroutines)

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			results <- repo.reserve("contested-job")
		}()
	}

	wg.Wait()
	close(results)

	successCount := 0
	for err := range results {
		if err == nil {
			successCount++
		}
	}

	if successCount != 1 {
		t.Errorf("Expected exactly 1 successful reserve, got %d", successCount)
	}
}

func TestStateRepo_ConcurrentReadWrite(t *testing.T) {
	t.Parallel()
	repo := newStateRepo()

	// Pre-populate some jobs
	for i := 0; i < 10; i++ {
		repo.commit(string(rune('a'+i)), &jobState{jobContainerID: string(rune('a' + i))})
	}

	// Concurrent reads and writes
	var wg sync.WaitGroup
	const numOps = 100

	// Readers
	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = repo.list()
			_ = repo.ids()
			_, _ = repo.get("a")
		}()
	}

	// Writers
	for i := 0; i < numOps; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			jobID := string(rune('A' + (i % 26)))
			if err := repo.reserve(jobID); err == nil {
				repo.commit(jobID, &jobState{jobContainerID: jobID})
			}
		}(i)
	}

	// This should complete without deadlock or panic
	wg.Wait()
}
