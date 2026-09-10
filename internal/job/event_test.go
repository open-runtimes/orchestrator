package job

import (
	"orchestrator/internal/callback"
	"strings"
	"testing"
	"unicode"
)

// assertSentence checks the human half of a failure: something to show, cased
// as a sentence, and naming what it is about.
func assertSentence(t *testing.T, message, mentions string) {
	t.Helper()
	if message == "" || !unicode.IsUpper([]rune(message)[0]) || !strings.Contains(message, mentions) {
		t.Errorf("message = %q, want a sentence mentioning %q", message, mentions)
	}
}

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

// The code is the branchable half and the message the human half. An
// unrecognized reason is never promoted into a specific code, and never
// forwarded either: old sidecars wrote diagnostics meant for their own logs.
//
// The message's wording is free to change; what is pinned is that it names the
// thing that failed and never carries the legacy diagnostic.
func TestBuildArtifactEventReportsFailureCode(t *testing.T) {
	for _, tc := range []struct{ name, reason, message, code, mentions string }{
		{"coded sidecar", "archive_empty", "archive source.tar.gz is empty", "archive_empty", "source.tar.gz"},
		{"legacy sidecar", "failed to open /private/source?token=secret", "", "archive_extraction_failed", "extract"},
		{"future sidecar", "new_archive_code", "", "archive_extraction_failed", "extract"},
		{"missing reason", "", "", "archive_extraction_failed", "extract"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := artifactEventData(t, &ArtifactReport{
				ID: "extract", Type: "unarchive", Status: "failed",
				FailureReason: tc.reason, FailureMessage: tc.message,
			})
			failure, _ := data["error"].(callback.Failure)
			if failure.Code != tc.code || data["status"] != "failed" {
				t.Fatalf("unexpected failure callback: %#v", data)
			}
			assertSentence(t, failure.Message, tc.mentions)
			if strings.Contains(failure.Message, "token=secret") {
				t.Errorf("legacy diagnostic reached the wire: %q", failure.Message)
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
