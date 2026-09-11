package artifact

import (
	"net/http"
	"orchestrator/internal/apperrors"
	"testing"
)

// An artifact of a type this service does not know is the request's fault, so
// it must reach the caller as a 400.
func TestValidate_UnknownTypeIsAValidationError(t *testing.T) {
	t.Parallel()
	err := Validate(0, &unknownArtifact{})
	if err == nil {
		t.Fatal("unknown type accepted")
	}
	if got := apperrors.HTTPStatus(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (%v)", got, http.StatusBadRequest, err)
	}
}

type unknownArtifact struct{ Write }

func (*unknownArtifact) ArtifactID() string   { return "x" }
func (*unknownArtifact) ArtifactType() string { return "unknown" }

// Apply is not the mount mechanism, and saying "success" without mounting
// anything is how a dropped mount looked like a working one.
func TestMountApply_FailsRatherThanClaimingSuccess(t *testing.T) {
	t.Parallel()
	res := (&Mount{ID: "data", In: "data.sqfs", Out: "data"}).Apply(t.Context(), t.TempDir())
	if res.Status != "failed" || res.Error == nil {
		t.Errorf("got %+v", res)
	}
}
