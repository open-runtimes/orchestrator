package job

import (
	"orchestrator/internal/callback"
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

// The code is the branchable half and the message the human half: an
// unrecognized reason must never be promoted into a specific code, but it is
// still detail, so it survives as the message.
func TestBuildArtifactEventReportsFailureCode(t *testing.T) {
	for _, tc := range []struct{ name, reason, message, code, want string }{
		{"coded sidecar", "archive_empty", "archive source.tar.gz is empty", "archive_empty", "Archive source.tar.gz is empty"},
		{"legacy sidecar", "failed to open source", "", "archive_extraction_failed", "Failed to open source"},
		{"future sidecar", "new_archive_code", "", "archive_extraction_failed", "New_archive_code"},
		{"missing reason", "", "", "archive_extraction_failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := artifactEventData(t, &ArtifactReport{
				ID: "extract", Type: "unarchive", Status: "failed",
				FailureReason: tc.reason, FailureMessage: tc.message,
			})
			want := callback.Failure{Code: tc.code, Message: tc.want}
			if data["error"] != want || data["status"] != "failed" {
				t.Fatalf("unexpected failure callback: %#v", data)
			}
		})
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
