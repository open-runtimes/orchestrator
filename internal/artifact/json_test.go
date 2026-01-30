package artifact

import (
	"encoding/json"
	"testing"
)

func TestUnmarshalArtifact(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantType string
		wantID   string
	}{
		{
			name:     "download",
			json:     `{"type":"download","id":"dl1","in":"https://example.com/file","out":"input.txt"}`,
			wantType: "download",
			wantID:   "dl1",
		},
		{
			name:     "upload",
			json:     `{"type":"upload","id":"ul1","in":"output.txt","out":"https://example.com/upload"}`,
			wantType: "upload",
			wantID:   "ul1",
		},
		{
			name:     "write",
			json:     `{"type":"write","id":"w1","in":"{}","out":"config.json"}`,
			wantType: "write",
			wantID:   "w1",
		},
		{
			name:     "read",
			json:     `{"type":"read","id":"r1","in":"result.json"}`,
			wantType: "read",
			wantID:   "r1",
		},
		{
			name:     "archive",
			json:     `{"type":"archive","id":"a1","in":"src","out":"src.tar.gz","format":"tar.gz"}`,
			wantType: "archive",
			wantID:   "a1",
		},
		{
			name:     "unarchive",
			json:     `{"type":"unarchive","id":"ua1","in":"src.tar.gz","out":"src"}`,
			wantType: "unarchive",
			wantID:   "ua1",
		},
		{
			name:     "unarchive with subdir",
			json:     `{"type":"unarchive","id":"ua2","in":"repo.tar.gz","out":"code","subdir":"functions/node"}`,
			wantType: "unarchive",
			wantID:   "ua2",
		},
		{
			name:     "list",
			json:     `{"type":"list","id":"l1","in":"src"}`,
			wantType: "list",
			wantID:   "l1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			artifact, err := UnmarshalArtifact([]byte(tt.json))
			if err != nil {
				t.Fatalf("UnmarshalArtifact() error = %v", err)
			}
			if artifact.ArtifactType() != tt.wantType {
				t.Errorf("ArtifactType() = %v, want %v", artifact.ArtifactType(), tt.wantType)
			}
			if artifact.ArtifactID() != tt.wantID {
				t.Errorf("ArtifactID() = %v, want %v", artifact.ArtifactID(), tt.wantID)
			}
		})
	}
}

func TestUnmarshalArtifact_UnknownType(t *testing.T) {
	_, err := UnmarshalArtifact([]byte(`{"type":"unknown","id":"x"}`))
	if err == nil {
		t.Error("Expected error for unknown type")
	}
}

func TestUnmarshalArtifact_InvalidJSON(t *testing.T) {
	_, err := UnmarshalArtifact([]byte(`{invalid`))
	if err == nil {
		t.Error("Expected error for invalid JSON")
	}
}

func TestUnmarshalArtifacts(t *testing.T) {
	input := `[
		{"type":"download","id":"dl1","in":"https://example.com/file","out":"input.txt"},
		{"type":"upload","id":"ul1","in":"output.txt","out":"https://example.com/upload","depends":"job"}
	]`

	artifacts, err := UnmarshalArtifacts([]byte(input))
	if err != nil {
		t.Fatalf("UnmarshalArtifacts() error = %v", err)
	}

	if len(artifacts) != 2 {
		t.Fatalf("Expected 2 artifacts, got %d", len(artifacts))
	}

	if artifacts[0].ArtifactID() != "dl1" {
		t.Errorf("First artifact ID = %v, want dl1", artifacts[0].ArtifactID())
	}
	if artifacts[1].ArtifactID() != "ul1" {
		t.Errorf("Second artifact ID = %v, want ul1", artifacts[1].ArtifactID())
	}
	if artifacts[1].DependsOn() != "job" {
		t.Errorf("Second artifact DependsOn() = %v, want job", artifacts[1].DependsOn())
	}
}

func TestUnmarshalArtifacts_InvalidArray(t *testing.T) {
	_, err := UnmarshalArtifacts([]byte(`not an array`))
	if err == nil {
		t.Error("Expected error for invalid array")
	}
}

func TestMarshalArtifact(t *testing.T) {
	original := &Download{
		ID:      "dl1",
		In:      "https://example.com/file",
		Out:     "input.txt",
		Depends: "other",
	}

	data, err := MarshalArtifact(original)
	if err != nil {
		t.Fatalf("MarshalArtifact() error = %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Failed to unmarshal to map: %v", err)
	}
	if m["type"] != "download" {
		t.Errorf("type = %v, want download", m["type"])
	}
	if m["id"] != "dl1" {
		t.Errorf("id = %v, want dl1", m["id"])
	}
}

func TestMarshalArtifact_RoundTrip(t *testing.T) {
	original := &Download{
		ID:      "dl1",
		In:      "https://example.com/file",
		Out:     "input.txt",
		Depends: "other",
	}

	data, err := MarshalArtifact(original)
	if err != nil {
		t.Fatalf("MarshalArtifact() error = %v", err)
	}

	artifact, err := UnmarshalArtifact(data)
	if err != nil {
		t.Fatalf("UnmarshalArtifact() error = %v", err)
	}

	dl, ok := artifact.(*Download)
	if !ok {
		t.Fatalf("Expected *Download, got %T", artifact)
	}
	if dl.ID != original.ID || dl.In != original.In || dl.Out != original.Out || dl.Depends != original.Depends {
		t.Errorf("Round-trip mismatch: got %+v, want %+v", dl, original)
	}
}

func TestMarshalArtifacts(t *testing.T) {
	artifacts := []Artifact{
		&Download{ID: "dl1", In: "https://example.com/file", Out: "input.txt"},
		&Upload{ID: "ul1", In: "output.txt", Out: "https://example.com/upload"},
	}

	data, err := MarshalArtifacts(artifacts)
	if err != nil {
		t.Fatalf("MarshalArtifacts() error = %v", err)
	}

	result, err := UnmarshalArtifacts(data)
	if err != nil {
		t.Fatalf("UnmarshalArtifacts() error = %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Expected 2 artifacts, got %d", len(result))
	}
}
