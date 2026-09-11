package testutil

import (
	"encoding/json"
	"orchestrator/internal/cloudevent"
	"testing"
)

// WireData returns the event's data as a subscriber receives it: marshaled to
// JSON and decoded generically, so tests assert on wire keys and omitted
// fields rather than on Go struct fields. Numbers come back as float64.
func WireData(tb testing.TB, e *cloudevent.Event) map[string]any {
	tb.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		tb.Fatalf("marshal event: %v", err)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		tb.Fatalf("decode event: %v", err)
	}
	return env.Data
}
