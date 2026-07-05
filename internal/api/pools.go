package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"orchestrator/internal/dispatcher"
	"orchestrator/pkg/cloudevent"
	"orchestrator/pkg/pool"
	"time"
)

// poolsHandler serves /v1/deployment-pools. Pools are config-defined, so the
// surface is read + activate only.
type poolsHandler struct {
	svc   *pool.Service
	queue dispatcher.Queue
}

// registerPoolRoutes mounts the pool routes when pools are configured.
func registerPoolRoutes(mux *http.ServeMux, auth func(http.Handler) http.Handler, svc *pool.Service, queue dispatcher.Queue) {
	h := &poolsHandler{svc: svc, queue: queue}
	mux.Handle("GET /v1/deployment-pools", auth(http.HandlerFunc(h.list)))
	mux.Handle("GET /v1/deployment-pools/{id}", auth(http.HandlerFunc(h.get)))
	mux.Handle("POST /v1/deployment-pools/{id}/activate", auth(http.HandlerFunc(h.activate)))
	mux.Handle("GET /v1/deployment-pools/{id}/activations", auth(http.HandlerFunc(h.activations)))
	mux.Handle("GET /v1/deployment-pools/{id}/activations/{actId}", auth(http.HandlerFunc(h.activation)))
	mux.Handle("DELETE /v1/deployment-pools/{id}/activations/{actId}", auth(http.HandlerFunc(h.deactivate)))
}

func (h *poolsHandler) list(w http.ResponseWriter, r *http.Request) {
	pools, err := h.svc.Pools(r.Context())
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": pools})
}

func (h *poolsHandler) get(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Pool(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// activate handles POST /v1/deployment-pools/{id}/activate. Sync by default:
// the call blocks and returns the result inline (exec: exit code + output;
// HTTP: the serving URL). `Prefer: respond-async` returns 202 immediately and
// delivers the result as an orchestrator.pool.activation.result CloudEvent —
// the callback is then required, since nothing is stored or pollable
// in-flight.
func (h *poolsHandler) activate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var act pool.Activation
	if err := json.NewDecoder(r.Body).Decode(&act); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	poolID := r.PathValue("id")

	// Exact-literal match by design; combined RFC 7240 forms are not recognized.
	if r.Header.Get("Prefer") != "respond-async" {
		status, err := h.svc.Activate(r.Context(), poolID, &act)
		if err != nil {
			handleServiceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
		return
	}

	if act.Callback == nil || act.Callback.URL == "" {
		writeError(w, http.StatusBadRequest, "async activation requires a callback")
		return
	}
	go h.activateAsync(poolID, &act)
	writeJSON(w, http.StatusAccepted, map[string]string{"poolId": poolID, "status": pool.StateActivating})
}

// activateAsync runs the activation detached and delivers the .result event.
// At-most-once: a crash mid-flight drops the callback (failure-semantics).
func (h *poolsHandler) activateAsync(poolID string, act *pool.Activation) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(act.TimeoutSeconds+120)*time.Second)
	defer cancel()

	status, err := h.svc.Activate(ctx, poolID, act)
	data := map[string]any{"poolId": poolID}
	if err != nil {
		data["activationId"] = act.ID
		data["status"] = pool.StateFailed
		data["error"] = err.Error()
	} else {
		data["activationId"] = status.ID
		data["status"] = status.State
		if status.ExitCode != nil {
			data["exitCode"] = *status.ExitCode
		}
		if status.Output != "" {
			data["output"] = status.Output
		}
		if status.URL != "" {
			data["url"] = status.URL
		}
		if status.Error != "" {
			data["error"] = status.Error
		}
	}

	event := cloudevent.New("orchestrator.pool.activation.result", "orchestrator/deployments", poolID, act.ID, data)
	if err := h.queue.Dispatch(&dispatcher.Event{
		Payload:     event,
		Destination: act.Callback.URL,
		SigningKey:  act.Callback.Key,
	}); err != nil {
		slog.Warn("Failed to dispatch activation result", "poolId", poolID, "activationId", act.ID, "error", err)
	}
}

func (h *poolsHandler) activations(w http.ResponseWriter, r *http.Request) {
	activations, err := h.svc.List(r.Context(), r.PathValue("id"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"activations": activations})
}

func (h *poolsHandler) activation(w http.ResponseWriter, r *http.Request) {
	status, err := h.svc.Status(r.Context(), r.PathValue("id"), r.PathValue("actId"))
	if err != nil {
		handleServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *poolsHandler) deactivate(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Deactivate(r.Context(), r.PathValue("id"), r.PathValue("actId")); err != nil {
		handleServiceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
