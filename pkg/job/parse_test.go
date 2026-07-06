package job

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"id": "j1", "image": "x", "command": "true", "timeoutSecnods": 60}`))
	if err == nil || !strings.Contains(err.Error(), "timeoutSecnods") {
		t.Fatalf("want unknown-field error naming the typo, got %v", err)
	}
}

// Stored specs (labels, annotations) must stay LENIENT: a spec written by a
// newer release must still decode on this one.
func TestUnmarshalToleratesUnknownFields(t *testing.T) {
	var r Request
	if err := json.Unmarshal([]byte(`{"id": "j1", "image": "x", "fieldFromTheFuture": 1}`), &r); err != nil {
		t.Fatalf("stored-spec decode must tolerate unknown fields: %v", err)
	}
}
