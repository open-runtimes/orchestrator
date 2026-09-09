package sidecar

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"orchestrator/internal/callback"
	"orchestrator/internal/job"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveFailureCallback(t *testing.T) {
	var truncated bytes.Buffer
	gz := gzip.NewWriter(&truncated)
	// A valid compressed stream containing an incomplete tar header.
	_, _ = gz.Write([]byte("incomplete tar"))
	_ = gz.Close()
	archive := func(name string) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 4}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("test")); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	// message is a substring the callback's human-readable half must carry, so
	// a subscriber can tell which file or entry failed without the sidecar logs.
	for _, tc := range []struct {
		name    string
		source  []byte
		code    string
		message string
		subdir  string
	}{
		// Gzip header followed by a reserved DEFLATE block type (BTYPE=3).
		{"corrupt deflate", []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 7}, "archive_corrupt", "corrupt input", ""},
		{"truncated tar", truncated.Bytes(), "archive_corrupt", "unexpected EOF", ""},
		{"empty archive", []byte{}, "archive_empty", "source.tar.gz is empty", ""},
		{"missing archive", nil, "artifact_not_found", "no such file or directory", ""},
		{"unknown format", []byte("not an archive"), "archive_unknown_format", "archive format for source.tar.gz", ""},
		{"invalid path", archive("../outside"), "archive_path_invalid", "../outside", ""},
		{"missing root directory", archive("package.json"), "archive_layout_mismatch", `subdir="nonexistent"`, "nonexistent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.source != nil {
				if err := os.WriteFile(filepath.Join(dir, "source.tar.gz"), tc.source, 0600); err != nil {
					t.Fatal(err)
				}
			}
			events := make(chan map[string]any, 2)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var report job.ArtifactReport
				if err := json.NewDecoder(req.Body).Decode(&report); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				events <- job.NewEventBuilder(report.JobID, "orchestrator/sidecar", report.Meta).BuildArtifactEvent(&report).Data
				w.WriteHeader(http.StatusOK)
			}))
			defer endpoint.Close()
			runner := NewRunner("test-build", dir, 10, artifact.DefaultRegistry(),
				WithArtifactListener(NewHTTPSink("test-build", endpoint.URL, "", time.Second, "", "", nil, map[string]string{"deploymentId": "deployment"})))
			err := runner.RunPre(context.Background(), []artifact.Artifact{
				&artifact.Unarchive{ID: "extract", In: "source.tar.gz", Out: "source", Subdir: tc.subdir},
				&artifact.Stat{ID: "after-extract", In: "source", Depends: "extract"},
			})
			if err == nil {
				t.Fatal("bad source must abort pre-job setup")
			}
			select {
			case data := <-events:
				failure, _ := data["error"].(callback.Failure)
				if data["status"] != "failed" || data["artifactId"] != "extract" || data["artifactType"] != "unarchive" ||
					failure.Code != tc.code || !strings.Contains(failure.Message, tc.message) {
					t.Fatalf("failure detail was lost across the sidecar HTTP report and callback: %#v", data)
				}
			default:
				t.Fatal("no artifact callback received")
			}
			if len(events) != 0 {
				t.Fatal("dependent artifact ran after extraction failed")
			}
			if CheckReady(dir) {
				t.Fatal("failed extraction allowed the worker to start")
			}
			entries, _ := os.ReadDir(filepath.Join(dir, "source"))
			if len(entries) != 0 {
				t.Fatal("invalid archive left source files behind")
			}
		})
	}
}
