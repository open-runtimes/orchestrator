package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestReady(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("path = %q, want /readyz", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	port := mustPort(t, srv.URL)

	if !ready(port) {
		t.Error("ready() = false with a 200 /readyz")
	}
	status = http.StatusServiceUnavailable
	if ready(port) {
		t.Error("ready() = true with a 503 /readyz")
	}
	srv.Close()
	if ready(port) {
		t.Error("ready() = true with nothing listening")
	}
}

func mustPort(t *testing.T, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}
