package job

import (
	"orchestrator/internal/artifact"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	svc := &Service{}

	tests := []struct {
		name    string
		req     *Request
		wantErr bool
		errMsg  string
	}{
		{
			name:    "empty ID",
			req:     &Request{Image: "alpine"},
			wantErr: true,
			errMsg:  "job ID is required",
		},
		{
			name:    "empty image",
			req:     &Request{ID: "test-job"},
			wantErr: true,
			errMsg:  "image is required",
		},
		{
			name: "valid minimal request",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
			},
			wantErr: false,
		},
		{
			name: "artifact without ID",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Upload{In: "output", Out: "http://example.com/upload"},
				},
			},
			wantErr: true,
			errMsg:  "id is required",
		},
		{
			name: "upload artifact without out (url)",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Upload{ID: "a1", In: "output"},
				},
			},
			wantErr: true,
			errMsg:  "out (url) is required",
		},
		{
			name: "valid request with artifacts",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Upload{ID: "a1", In: "output", Out: "http://example.com/upload", Depends: artifact.JobDependency},
					&artifact.Read{ID: "a2", In: "result.json", Depends: artifact.JobDependency},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := svc.validate(tt.req)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error containing %q", tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	t.Parallel()
	req := &Request{
		ID:    "test-job",
		Image: "alpine",
	}

	applyDefaults(req)

	// Check defaults were set
	if req.TimeoutSeconds != 1800 {
		t.Errorf("Expected default timeout 1800, got %d", req.TimeoutSeconds)
	}
	if req.CPU != 1 {
		t.Errorf("Expected default CPU 1, got %v", req.CPU)
	}
	if req.Memory != 512 {
		t.Errorf("Expected default memory 512, got %d", req.Memory)
	}
}

func TestApplyDefaults_PreservesExisting(t *testing.T) {
	t.Parallel()
	req := &Request{
		ID:             "test-job",
		Image:          "alpine",
		TimeoutSeconds: 3600,
		CPU:            4,
		Memory:         2048,
	}

	applyDefaults(req)

	// Check existing values were preserved
	if req.TimeoutSeconds != 3600 {
		t.Errorf("Expected preserved timeout 3600, got %d", req.TimeoutSeconds)
	}
	if req.CPU != 4 {
		t.Errorf("Expected preserved CPU 4, got %v", req.CPU)
	}
	if req.Memory != 2048 {
		t.Errorf("Expected preserved memory 2048, got %d", req.Memory)
	}
}

func TestValidate_Artifacts(t *testing.T) {
	t.Parallel()
	svc := &Service{}

	tests := []struct {
		name    string
		req     *Request
		wantErr bool
		errMsg  string
	}{
		{
			name: "download artifact without ID",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Download{Out: "input.txt", In: "http://example.com/file"},
				},
			},
			wantErr: true,
			errMsg:  "id is required",
		},
		{
			name: "download artifact without out (path)",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Download{ID: "a1", In: "http://example.com/file"},
				},
			},
			wantErr: true,
			errMsg:  "out (path) is required",
		},
		{
			name: "download artifact without in (url)",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Download{ID: "a1", Out: "input.txt"},
				},
			},
			wantErr: true,
			errMsg:  "in (url) is required",
		},
		{
			name: "write artifact without in (content)",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Write{ID: "a1", Out: "config.json"},
				},
			},
			wantErr: true,
			errMsg:  "in (content) is required",
		},
		{
			name: "valid download artifact",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Download{ID: "a1", Out: "input.txt", In: "http://example.com/file"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid write artifact",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Write{ID: "a1", Out: "config.json", In: `{"key": "value"}`},
				},
			},
			wantErr: false,
		},
		{
			name: "valid archive artifact",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Archive{ID: "a1", In: "output", Out: "output.tar.gz", Format: "tar.gz", Depends: artifact.JobDependency},
				},
			},
			wantErr: false,
		},
		{
			name: "archive without out (dest)",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Archive{ID: "a1", In: "output", Format: "tar.gz", Depends: artifact.JobDependency},
				},
			},
			wantErr: true,
			errMsg:  "out (dest) is required",
		},
		{
			name: "unarchive without out (dest)",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Unarchive{ID: "a1", In: "input.tar.gz"},
				},
			},
			wantErr: true,
			errMsg:  "out (dest) is required",
		},
		{
			name: "valid unarchive artifact",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Unarchive{ID: "a1", In: "input.tar.gz", Out: "code"},
				},
			},
			wantErr: false,
		},
		{
			name: "multiple artifacts mixed valid",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Download{ID: "a1", Out: "data.csv", In: "http://example.com/data"},
					&artifact.Write{ID: "a2", Out: "config.json", In: `{}`},
				},
			},
			wantErr: false,
		},
		{
			name: "path traversal attack",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Write{ID: "a1", Out: "../../../etc/passwd", In: "malicious"},
				},
			},
			wantErr: true,
			errMsg:  "path traversal",
		},
		{
			name: "absolute path not allowed",
			req: &Request{
				ID:    "test-job",
				Image: "alpine",
				Artifacts: []artifact.Artifact{
					&artifact.Write{ID: "a1", Out: "/etc/passwd", In: "malicious"},
				},
			},
			wantErr: true,
			errMsg:  "path must be relative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := svc.validate(tt.req)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Expected error containing %q", tt.errMsg)
				} else if !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Expected error containing %q, got %q", tt.errMsg, err.Error())
				}
			} else if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}
}

func TestUnmarshalArtifact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		json     string
		wantType string
		wantErr  bool
	}{
		{
			name:     "download artifact",
			json:     `{"type":"download","id":"a1","in":"http://example.com/file","out":"input.txt"}`,
			wantType: "download",
		},
		{
			name:     "upload artifact",
			json:     `{"type":"upload","id":"a1","in":"output.txt","out":"http://example.com/upload"}`,
			wantType: "upload",
		},
		{
			name:     "write artifact",
			json:     `{"type":"write","id":"a1","in":"{}","out":"config.json"}`,
			wantType: "write",
		},
		{
			name:     "read artifact",
			json:     `{"type":"read","id":"a1","in":"result.json"}`,
			wantType: "read",
		},
		{
			name:     "archive artifact",
			json:     `{"type":"archive","id":"a1","in":"output","out":"output.tar.gz","format":"tar.gz"}`,
			wantType: "archive",
		},
		{
			name:     "unarchive artifact",
			json:     `{"type":"unarchive","id":"a1","in":"input.tar.gz","out":"code"}`,
			wantType: "unarchive",
		},
		{
			name:     "list artifact",
			json:     `{"type":"list","id":"a1","in":"output"}`,
			wantType: "list",
		},
		{
			name:    "invalid type",
			json:    `{"type":"invalid","id":"a1","in":"file.txt"}`,
			wantErr: true,
		},
		{
			name:    "missing type",
			json:    `{"id":"a1","in":"file.txt"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := artifact.UnmarshalArtifact([]byte(tt.json))
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if a.ArtifactType() != tt.wantType {
				t.Errorf("Expected type %q, got %q", tt.wantType, a.ArtifactType())
			}
		})
	}
}
