package job

import (
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

func artifactEventData(t *testing.T, r *ArtifactReport) ArtifactData {
	t.Helper()

	r.JobID = "job-1"
	return ArtifactEvent(r).Data.(ArtifactData)
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

	if data.Format != "squashfs" {
		t.Errorf("format = %v, want squashfs", data.Format)
	}
	if data.Compression != "lz4" {
		t.Errorf("compression = %v, want lz4", data.Compression)
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

	if data.Format != "" || data.Compression != "" {
		t.Errorf("format/compression = %q/%q, want omitted", data.Format, data.Compression)
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
			if data.Error == nil || data.Error.Code != tc.code || data.Status != "failed" {
				t.Fatalf("unexpected failure callback: %#v", data)
			}
			assertSentence(t, data.Error.Message, tc.mentions)
			if strings.Contains(data.Error.Message, "token=secret") {
				t.Errorf("legacy diagnostic reached the wire: %q", data.Error.Message)
			}
		})
	}
}

func TestBuildArtifactEventOmitsAbsentOptionalFields(t *testing.T) {
	data := artifactEventData(t, &ArtifactReport{ID: "code", Type: "download", Status: "success"})

	if data.Error != nil || data.Content != nil {
		t.Errorf("error/content = %v/%v, want omitted", data.Error, data.Content)
	}
	if data.ArtifactID != "code" {
		t.Errorf("artifactId = %v, want code", data.ArtifactID)
	}
	if data.JobID != "job-1" {
		t.Errorf("jobId = %v, want job-1", data.JobID)
	}
}
