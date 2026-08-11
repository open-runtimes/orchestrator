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

// Environment is a map of string to string, full stop: a non-string value
// (bool, number) must fail at decode, not be coerced.
func TestParseRejectsNonStringEnvValues(t *testing.T) {
	for _, v := range []string{"true", "1"} {
		_, err := Parse([]byte(`{"id": "j1", "image": "x", "command": "true", "environment": {"DEBUG": ` + v + `}}`))
		if err == nil {
			t.Fatalf("want decode error for environment value %s, got nil", v)
		}
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
