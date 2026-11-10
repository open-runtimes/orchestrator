package api

import (
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/artifact"
	"strings"
	"testing"
)

// The pods' routes live only on the internal router, so a gateway route to the
// API cannot reach them and a pod allowed to the internal port reaches nothing
// else.
func TestRouters_KeepPodRoutesOffTheAPI(t *testing.T) {
	t.Parallel()
	public := NewOrchestratorRouter(OrchestratorRouterConfig{})
	internal := NewInternalRouter(OrchestratorRouterConfig{})

	presign := func(h http.Handler) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, artifact.PresignPath, strings.NewReader("{")))
		return w.Code
	}
	if got := presign(public); got != http.StatusNotFound {
		t.Errorf("API presign: status %d, want 404", got)
	}
	if got := presign(internal); got != http.StatusBadRequest {
		t.Errorf("internal presign: status %d, want 400 for a malformed body", got)
	}

	w := httptest.NewRecorder()
	internal.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("internal /readyz: status %d, want 404", w.Code)
	}
}
