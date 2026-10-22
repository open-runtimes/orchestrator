package sandbox

import (
	"encoding/json"
	"orchestrator/internal/artifact"
	"strings"
	"testing"
)

func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Recorded() is stored on the pod as an annotation. It must describe the
// serving shape and carry nothing secret: the command, the environment, and
// the artifacts (whose download headers hold the source credentials) reach the
// workload over the claim protocol, not the annotation.
func TestRecorded_DropsSecretMaterial(t *testing.T) {
	req := &Request{
		ID:          "sbx-1",
		Image:       "node:20",
		Port:        3000,
		Command:     "run-secret-build",
		Environment: map[string]string{"API_KEY": "s3cr3t"},
		Artifacts: artifact.Set{
			&artifact.Download{ID: "src", In: "https://example.test/src.tgz", Out: "src.tgz",
				Headers: map[string]string{"Authorization": "Bearer deadbeef"}},
		},
	}

	rec := req.Recorded(req.Shape())

	if rec.Command != "" || rec.Environment != nil || rec.Artifacts != nil {
		t.Errorf("Recorded() kept secret material: command=%q env=%v artifacts=%v", rec.Command, rec.Environment, rec.Artifacts)
	}
	if rec.Image != "node:20" || rec.Port != 3000 {
		t.Errorf("Recorded() lost the serving shape: image=%q port=%d", rec.Image, rec.Port)
	}

	for _, secret := range []string{"s3cr3t", "deadbeef", "run-secret-build"} {
		if strings.Contains(marshalJSON(t, rec), secret) {
			t.Errorf("Recorded() JSON leaks %q", secret)
		}
	}
}
