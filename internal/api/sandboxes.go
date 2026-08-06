package api

import (
	"net/http"
	"orchestrator/internal/artifact"
	"orchestrator/internal/sandbox"
)

// sandboxesHandler serves /v1/sandbox and /v1/sandbox-pool. Pools are
// config-defined, so the surface over them is read-only; sandboxes themselves
// are created and torn down by callers.
//
// Exec and files are deliberately absent: they are served by the sandbox image
// at the sandbox's own URL, which keeps this control plane off the data path.
type sandboxesHandler struct {
	svc *sandbox.Service
}

// registerSandboxRoutes mounts the sandbox routes when sandbox pools are
// configured.
func registerSandboxRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler, svc *sandbox.Service) {
	h := &sandboxesHandler{svc: svc}
	mux.Handle("POST /v1/sandbox", auth(http.HandlerFunc(h.create)))
	mux.Handle("GET /v1/sandbox", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/sandbox/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("DELETE /v1/sandbox/{id}", auth(http.HandlerFunc(h.remove)))
	mux.Handle("GET /v1/sandbox-pool", auth(http.HandlerFunc(h.pools)))
	mux.Handle("GET /v1/sandbox-pool/{id}", auth(http.HandlerFunc(h.pool)))
}

// create handles POST /v1/sandbox. Synchronous by design: a claim is
// sub-second, and the response carries the sandbox's URL — which is live when
// it is returned, since there is no per-sandbox gateway route to wait on.
func (h *sandboxesHandler) create(w http.ResponseWriter, r *http.Request) {
	req, ok := parseBody(w, r, parseSandbox)
	if !ok {
		return
	}
	status, err := h.svc.Create(r.Context(), req)
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, status)
}

// parseSandbox decodes a create body, rejecting unknown fields — a typo'd
// field must fail loudly, not silently create a sandbox with defaults.
func parseSandbox(data []byte) (*sandbox.Request, error) {
	var req sandbox.Request
	if err := artifact.UnmarshalStrict(data, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func (h *sandboxesHandler) list(w http.ResponseWriter, r *http.Request) {
	sandboxes, err := h.svc.List(r.Context())
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sandbox.ListResponse{Sandboxes: sandboxes})
}

func (h *sandboxesHandler) get(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Status(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// remove tears a sandbox down. 202 when its teardown has post-phase artifacts to
// run — the caller can watch for `finalizing` to clear — and 204 when there was
// nothing to wait for, so the common case still means "gone".
func (h *sandboxesHandler) remove(w http.ResponseWriter, r *http.Request) {
	finalizing, err := h.svc.Delete(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	if finalizing {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *sandboxesHandler) pools(w http.ResponseWriter, r *http.Request) {
	pools, err := h.svc.Pools(r.Context())
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

func (h *sandboxesHandler) pool(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Pool(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
