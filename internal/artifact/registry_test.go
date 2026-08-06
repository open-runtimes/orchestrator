package artifact

import (
	"net/http"
	"orchestrator/internal/apperrors"
	"strings"
	"testing"
)

// A mount cannot work without a post-phase sidecar to establish it and undo it.
// Serving workloads run no post phase, so the request has to FAIL — being
// accepted and dropped is what it used to do, and the caller had no way to
// learn its mount never happened.
func TestServingRegistry_RejectsAMountInsteadOfDroppingIt(t *testing.T) {
	t.Parallel()
	mount := &Mount{ID: "data", In: "data.sqfs", Out: "data"}

	err := ServingRegistry().Validate(0, mount)
	if err == nil {
		t.Fatal("a serving workload cannot honour a mount; want an error")
	}
	// It must reach the caller as a 400, not a 500: the request is the problem.
	if got := apperrors.HTTPStatus(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (%v)", got, http.StatusBadRequest, err)
	}
	if !strings.Contains(err.Error(), "before the workload starts") {
		t.Errorf("the error should say what is missing, got %q", err)
	}

	// Jobs run the post phase, so the same artifact is fine there.
	if err := DefaultRegistry().Validate(0, mount); err != nil {
		t.Errorf("jobs can mount: %v", err)
	}
}

// Only the types that need a post phase are affected; everything a serving
// workload can materialize still validates.
func TestServingRegistry_AcceptsEverythingElse(t *testing.T) {
	t.Parallel()
	artifacts := []Artifact{
		&Download{ID: "src", In: "https://example.com/a.tgz", Out: "a.tgz"},
		&Write{ID: "cfg", In: "x", Out: "cfg.txt"},
		&Unarchive{ID: "unpack", In: "a.tgz", Out: "src"},
	}
	for i, a := range artifacts {
		if err := ServingRegistry().Validate(i, a); err != nil {
			t.Errorf("%s: %v", a.ArtifactType(), err)
		}
	}
}

// Both registries carry the same type set, so a type is never silently unknown
// on one plane and understood on the other — the difference is only whether it
// can be honoured, which is a validation error with a reason.
func TestRegistries_KnowTheSameTypes(t *testing.T) {
	t.Parallel()
	for _, td := range builtinTypes() {
		if _, ok := DefaultRegistry().types[td.Type]; !ok {
			t.Errorf("%q missing from the jobs registry", td.Type)
		}
		if _, ok := ServingRegistry().types[td.Type]; !ok {
			t.Errorf("%q missing from the serving registry", td.Type)
		}
	}
}

// Apply is not the mount mechanism, and saying "success" without mounting
// anything is how a dropped mount looked like a working one.
func TestMountApply_FailsRatherThanClaimingSuccess(t *testing.T) {
	t.Parallel()
	res := (&Mount{ID: "data", In: "data.sqfs", Out: "data"}).Apply(t.Context(), t.TempDir())
	if res.Status != "error" || res.Error == nil {
		t.Errorf("got %+v", res)
	}
}
