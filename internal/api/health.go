package api

import (
	"net/http"
	"orchestrator/internal/health"
)

// registerHealthRoutes mounts the liveness/readiness probes — no auth, kubelet
// calls them.
func registerHealthRoutes(mux *http.ServeMux, checker *health.Checker) {
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, checker.Liveness(r.Context()))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		response := checker.Readiness(r.Context())
		status := http.StatusOK
		if !response.IsHealthy() {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, response)
	})
}
