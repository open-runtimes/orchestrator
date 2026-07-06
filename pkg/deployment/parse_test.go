package deployment

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"id": "web", "image": "x", "port": 8080, "replcias": 3}`))
	if err == nil || !strings.Contains(err.Error(), "replcias") {
		t.Fatalf("want unknown-field error naming the typo, got %v", err)
	}
}

// Stored specs (Spec Secrets, volume labels) must stay LENIENT: a spec
// written by a newer release must still decode on this one.
func TestUnmarshalToleratesUnknownFields(t *testing.T) {
	var r Request
	if err := json.Unmarshal([]byte(`{"id": "web", "image": "x", "fieldFromTheFuture": 1}`), &r); err != nil {
		t.Fatalf("stored-spec decode must tolerate unknown fields: %v", err)
	}
	if r.ID != "web" {
		t.Errorf("decode dropped known fields: %+v", r)
	}
}
