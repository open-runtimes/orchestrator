package proxy

import (
	"net/http"
	"regexp"
	"testing"
)

// The Go-side matcher and the gateway regex are one protocol decision; if
// they ever disagree, sync/async traffic silently splits between the data
// plane and the API.
func TestPreferAsyncMatchersAgree(t *testing.T) {
	re := regexp.MustCompile(PreferAsyncPattern)
	cases := []struct {
		value string
		async bool
	}{
		{"respond-async", true},
		{"Respond-Async", true},
		{"RESPOND-ASYNC", true},
		{"respond-async, wait=10", false},
		{"wait=10", false},
		{"", false},
		{"respond-asyncx", false},
	}
	for _, tc := range cases {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://h/", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if tc.value != "" {
			req.Header.Set("Prefer", tc.value)
		}
		if got := PreferAsync(req); got != tc.async {
			t.Errorf("PreferAsync(%q) = %v, want %v", tc.value, got, tc.async)
		}
		if got := re.MatchString(tc.value); got != tc.async {
			t.Errorf("pattern match(%q) = %v, want %v — the matchers disagree", tc.value, got, tc.async)
		}
	}
}
