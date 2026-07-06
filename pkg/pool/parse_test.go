package pool

import (
	"encoding/json"
	"strings"
	"testing"
)

// Activations carry artifacts through two codecs: the API decode (Parse) and
// the pod-annotation round trip (MarshalJSON → UnmarshalJSON). Both must
// route artifacts through the registry — a plain decode cannot land on the
// interface, which is exactly how API activations with artifacts were broken
// before this codec existed.
func TestParseDecodesArtifacts(t *testing.T) {
	act, err := Parse([]byte(`{
		"id": "act1",
		"command": "cat hello.txt",
		"artifacts": [{"type": "write", "path": "hello.txt", "content": "hi"}]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(act.Artifacts) != 1 {
		t.Fatalf("artifacts: want 1, got %d", len(act.Artifacts))
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"command": "true", "timeotSeconds": 30}`))
	if err == nil || !strings.Contains(err.Error(), "timeotSeconds") {
		t.Fatalf("want unknown-field error naming the typo, got %v", err)
	}
}

func TestActivationAnnotationRoundTrip(t *testing.T) {
	act, err := Parse([]byte(`{
		"id": "act1",
		"command": "run",
		"environment": {"K": "v"},
		"artifacts": [{"type": "write", "path": "f", "content": "x"}]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The pod-annotation path: marshal (must stamp the "type" discriminator),
	// then reconstruct.
	data, err := json.Marshal(act)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"type":"write"`) {
		t.Fatalf("marshaled activation lost the artifact type discriminator: %s", data)
	}
	var back Activation
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Artifacts) != 1 {
		t.Fatalf("round trip lost artifacts: %+v", back.Artifacts)
	}
	if back.Command != "run" || back.Environment["K"] != "v" {
		t.Errorf("round trip mutated fields: %+v", back)
	}
}

// Stored specs must stay LENIENT: a pod annotation written by a newer
// release (extra fields) must still reconstruct on this one.
func TestUnmarshalToleratesUnknownFields(t *testing.T) {
	var act Activation
	if err := json.Unmarshal([]byte(`{"command": "run", "fieldFromTheFuture": 1}`), &act); err != nil {
		t.Fatalf("stored-spec decode must tolerate unknown fields: %v", err)
	}
}
