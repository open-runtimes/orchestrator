package artifact

import (
	"context"
	"errors"
	"testing"
)

// stub is a minimal Artifact for testing graph logic.
type stub struct {
	id      string
	depends string
}

func (s *stub) ArtifactID() string                        { return s.id }
func (s *stub) ArtifactType() string                      { return "stub" }
func (s *stub) DependsOn() string                         { return s.depends }
func (s *stub) Apply(_ context.Context, _ string) *Result { return &Result{Status: "success"} }

func TestPartition(t *testing.T) {
	tests := []struct {
		name        string
		artifacts   []Artifact
		wantPreIDs  []string
		wantPostIDs []string
	}{
		{
			name: "direct job dependency is post-job",
			artifacts: []Artifact{
				&stub{id: "a"},
				&stub{id: "b", depends: JobDependency},
			},
			wantPreIDs:  []string{"a"},
			wantPostIDs: []string{"b"},
		},
		{
			name: "transitive job dependency is post-job",
			artifacts: []Artifact{
				&stub{id: "download"},
				&stub{id: "extract", depends: "download"},
				&stub{id: "archive", depends: JobDependency},
				&stub{id: "upload", depends: "archive"},
			},
			wantPreIDs:  []string{"download", "extract"},
			wantPostIDs: []string{"archive", "upload"},
		},
		{
			name:        "empty list",
			artifacts:   nil,
			wantPreIDs:  nil,
			wantPostIDs: nil,
		},
		{
			name: "all pre-job",
			artifacts: []Artifact{
				&stub{id: "a"},
				&stub{id: "b", depends: "a"},
			},
			wantPreIDs:  []string{"a", "b"},
			wantPostIDs: nil,
		},
		{
			name: "all post-job",
			artifacts: []Artifact{
				&stub{id: "a", depends: JobDependency},
				&stub{id: "b", depends: "a"},
			},
			wantPreIDs:  nil,
			wantPostIDs: []string{"a", "b"},
		},
		{
			name: "input order preserved within each phase",
			artifacts: []Artifact{
				&stub{id: "z"},
				&stub{id: "y", depends: JobDependency},
				&stub{id: "x"},
				&stub{id: "w", depends: "y"},
			},
			wantPreIDs:  []string{"z", "x"},
			wantPostIDs: []string{"y", "w"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preJob, postJob := Partition(tt.artifacts)

			ids := func(arts []Artifact) []string {
				if arts == nil {
					return nil
				}
				out := make([]string, len(arts))
				for i, a := range arts {
					out[i] = a.ArtifactID()
				}
				return out
			}

			preIDs := ids(preJob)
			postIDs := ids(postJob)

			if len(preIDs) != len(tt.wantPreIDs) {
				t.Errorf("pre-job: got %v, want %v", preIDs, tt.wantPreIDs)
			} else {
				for i := range preIDs {
					if preIDs[i] != tt.wantPreIDs[i] {
						t.Errorf("pre-job[%d]: got %q, want %q", i, preIDs[i], tt.wantPreIDs[i])
					}
				}
			}

			if len(postIDs) != len(tt.wantPostIDs) {
				t.Errorf("post-job: got %v, want %v", postIDs, tt.wantPostIDs)
			} else {
				for i := range postIDs {
					if postIDs[i] != tt.wantPostIDs[i] {
						t.Errorf("post-job[%d]: got %q, want %q", i, postIDs[i], tt.wantPostIDs[i])
					}
				}
			}
		})
	}
}

func TestRunInOrder(t *testing.T) {
	t.Run("calls fn in dependency order", func(t *testing.T) {
		artifacts := []Artifact{
			&stub{id: "b", depends: "a"},
			&stub{id: "a"},
			&stub{id: "c", depends: "b"},
		}
		var order []string
		err := RunInOrder(t.Context(), artifacts, func(_ context.Context, a Artifact) error {
			order = append(order, a.ArtifactID())
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// a must come before b, b before c
		pos := func(id string) int {
			for i, s := range order {
				if s == id {
					return i
				}
			}
			return -1
		}
		if pos("a") >= pos("b") {
			t.Errorf("expected a before b, got order %v", order)
		}
		if pos("b") >= pos("c") {
			t.Errorf("expected b before c, got order %v", order)
		}
	})

	t.Run("job sentinel treated as satisfied", func(t *testing.T) {
		artifacts := []Artifact{
			&stub{id: "a", depends: JobDependency},
		}
		var called []string
		err := RunInOrder(t.Context(), artifacts, func(_ context.Context, a Artifact) error {
			called = append(called, a.ArtifactID())
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(called) != 1 || called[0] != "a" {
			t.Errorf("expected [a], got %v", called)
		}
	})

	t.Run("returns last error but continues executing independent artifacts", func(t *testing.T) {
		errA := errors.New("a failed")
		artifacts := []Artifact{
			&stub{id: "a"},
			&stub{id: "b"}, // independent of a
		}
		var called []string
		err := RunInOrder(t.Context(), artifacts, func(_ context.Context, a Artifact) error {
			called = append(called, a.ArtifactID())
			if a.ArtifactID() == "a" {
				return errA
			}
			return nil
		})
		if !errors.Is(err, errA) {
			t.Errorf("expected errA, got %v", err)
		}
		if len(called) != 2 {
			t.Errorf("expected both artifacts called, got %v", called)
		}
	})

	t.Run("skips artifacts in a cycle without hanging", func(t *testing.T) {
		artifacts := []Artifact{
			&stub{id: "a", depends: "b"},
			&stub{id: "b", depends: "a"},
		}
		var called []string
		_ = RunInOrder(t.Context(), artifacts, func(_ context.Context, a Artifact) error {
			called = append(called, a.ArtifactID())
			return nil
		})
		if len(called) != 0 {
			t.Errorf("expected no artifacts called for cycle, got %v", called)
		}
	})

	t.Run("empty list is a no-op", func(t *testing.T) {
		err := RunInOrder(t.Context(), nil, func(_ context.Context, a Artifact) error {
			t.Error("fn should not be called for empty list")
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
