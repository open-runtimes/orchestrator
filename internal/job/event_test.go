package job

import (
	"testing"
)

func artifactEventData(t *testing.T, r *ArtifactReport) map[string]any {
	t.Helper()

	return NewEventBuilder("job-1", "orchestrator/service", nil).BuildArtifactEvent(r).Data
}

// The artifact endpoint and the callback subscriber see the same report, so a
// field carried by one has to reach the other.
func TestBuildArtifactEventCarriesClassification(t *testing.T) {
	data := artifactEventData(t, &ArtifactReport{
		ID:          "code",
		Type:        "unarchive",
		Status:      "success",
		Format:      "squashfs",
		Compression: "lz4",
	})

	if data["format"] != "squashfs" {
		t.Errorf("format = %v, want squashfs", data["format"])
	}
	if data["compression"] != "lz4" {
		t.Errorf("compression = %v, want lz4", data["compression"])
	}
}

// Absent must stay distinguishable from a real value, so a subscriber can tell
// "could not be determined" from "genuinely uncompressed".
func TestBuildArtifactEventOmitsUnknownClassification(t *testing.T) {
	data := artifactEventData(t, &ArtifactReport{
		ID:     "code",
		Type:   "download",
		Status: "success",
	})

	if _, ok := data["format"]; ok {
		t.Errorf("format present as %v, want omitted", data["format"])
	}
	if _, ok := data["compression"]; ok {
		t.Errorf("compression present as %v, want omitted", data["compression"])
	}
}

func TestBuildArtifactEventReportsFailureReason(t *testing.T) {
	data := artifactEventData(t, &ArtifactReport{
		ID:            "code",
		Type:          "unarchive",
		Status:        "failed",
		FailureReason: "archive is empty",
	})

	if data["error"] != "archive is empty" {
		t.Errorf("error = %v, want %q", data["error"], "archive is empty")
	}
	if data["status"] != "failed" {
		t.Errorf("status = %v, want failed", data["status"])
	}
}

func TestBuildArtifactEventOmitsAbsentOptionalFields(t *testing.T) {
	data := artifactEventData(t, &ArtifactReport{ID: "code", Type: "download", Status: "success"})

	for _, key := range []string{"error", "content"} {
		if _, ok := data[key]; ok {
			t.Errorf("%s present as %v, want omitted", key, data[key])
		}
	}
	if data["artifactId"] != "code" {
		t.Errorf("artifactId = %v, want code", data["artifactId"])
	}
	if data["jobId"] != "job-1" {
		t.Errorf("jobId = %v, want job-1", data["jobId"])
	}
}
