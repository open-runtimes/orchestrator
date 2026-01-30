package dispatcher

import "testing"

func TestExtractHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rawURL   string
		expected string
	}{
		{
			name:     "standard URL with port",
			rawURL:   "http://localhost:8080/webhook",
			expected: "localhost:8080",
		},
		{
			name:     "HTTPS URL without port",
			rawURL:   "https://example.com/callback",
			expected: "example.com",
		},
		{
			name:     "URL with path and query",
			rawURL:   "http://api.example.com:3000/v1/events?key=123",
			expected: "api.example.com:3000",
		},
		{
			name:     "malformed URL returns raw input",
			rawURL:   "://invalid",
			expected: "://invalid",
		},
		{
			name:     "empty URL returns empty",
			rawURL:   "",
			expected: "",
		},
		{
			name:     "URL with IP address",
			rawURL:   "http://192.168.1.1:9000/hook",
			expected: "192.168.1.1:9000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractHost(tt.rawURL)
			if got != tt.expected {
				t.Errorf("extractHost(%q) = %q, want %q", tt.rawURL, got, tt.expected)
			}
		})
	}
}
