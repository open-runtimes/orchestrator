package pool

import (
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/artifact"
	"strings"
	"testing"
)

// An activation and a sandbox are the same thing to the warm layer — a claimed
// pod running a late-bound payload — so mounting follows the same rule for both:
// the pool decides, because the pod was built before the claim arrived.
func TestValidate_MountNeedsThePoolCapability(t *testing.T) {
	t.Parallel()
	svc := &Service{artifacts: artifact.MountingRegistry()}
	act := func() *Activation {
		return &Activation{
			ID:      "act",
			Command: "serve",
			Artifacts: artifact.Set{
				&artifact.Mount{ID: "data", In: "data.sqfs", Out: "data"},
			},
		}
	}

	err := svc.validate(&Pool{ID: "web", Spec: Spec{Port: 3000}}, act())
	if err == nil {
		t.Fatal("a pool that cannot mount must refuse the activation")
	}
	if got := apperrors.HTTPStatus(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "mounts on the pool") {
		t.Errorf("the error should name the pool setting, got %q", err)
	}

	if err := svc.validate(&Pool{ID: "sqfs", Spec: Spec{Port: 3000, Mounts: true}}, act()); err != nil {
		t.Errorf("a pool that declares mounts should accept one: %v", err)
	}
}

// Everything else an activation can carry is unaffected by the capability.
func TestValidate_AcceptsOrdinaryArtifactsWithoutTheCapability(t *testing.T) {
	t.Parallel()
	svc := &Service{artifacts: artifact.MountingRegistry()}

	err := svc.validate(&Pool{ID: "web", Spec: Spec{Port: 3000}}, &Activation{
		ID:      "act",
		Command: "serve",
		Artifacts: artifact.Set{
			&artifact.Download{ID: "src", In: "https://example.com/a.tgz", Out: "a.tgz"},
			&artifact.Unarchive{ID: "unpack", In: "a.tgz", Out: "src", Depends: "src"},
		},
	})
	if err != nil {
		t.Errorf("want accepted, got %v", err)
	}
}
