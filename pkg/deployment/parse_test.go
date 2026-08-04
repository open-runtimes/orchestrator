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

// The Spec Secret path: every field a caller set must survive
// marshal→unmarshal. A tier lost here is silent and dangerous — the workload
// would run on the shared host kernel while the caller believes it asked for
// gVisor.
func TestRequestSpecRoundTrip(t *testing.T) {
	req, err := Parse([]byte(`{
		"id": "web",
		"image": "img",
		"sandbox": "gvisor",
		"port": 8080,
		"replicas": 2,
		"environment": {"K": "v"},
		"artifacts": [{"type": "write", "out": "f", "in": "x"}]
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"type":"write"`) {
		t.Fatalf("marshaled spec lost the artifact type discriminator: %s", data)
	}
	var back Request
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Sandbox != "gvisor" {
		t.Errorf("round trip dropped the isolation tier: %+v", back)
	}
	if len(back.Artifacts) != 1 || back.Port != 8080 || back.Replicas != 2 || back.Environment["K"] != "v" {
		t.Errorf("round trip mutated fields: %+v", back)
	}
}
