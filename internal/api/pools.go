package api

import (
	"context"
	"log/slog"
	"net/http"
	"orchestrator/internal/dispatcher"
	"orchestrator/pkg/cloudevent"
	"orchestrator/pkg/pool"
	"strings"
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
	mux.Handle("POST /v1/deployment-pools/{id}/activations", auth(http.HandlerFunc(h.activate)))
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

// activate handles POST /v1/deployment-pools/{id}/activations. Sync by
// default: the call blocks and returns the result inline (exec: exit code +
// output; HTTP: the serving URL). `Prefer: respond-async` returns 202
// immediately and delivers the result as an
// orchestrator.pool.activation.result CloudEvent — the callback is then
// required, since nothing is stored or pollable in-flight.
func (h *poolsHandler) activate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	act, err := pool.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body: "+err.Error())
		return
	}
	poolID := r.PathValue("id")

	if !preferRespondAsync(r) {
		status, err := h.svc.Activate(r.Context(), poolID, act)
		if err != nil {
			handleServiceError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, status)
		return
	}

	if act.Callback == nil || act.Callback.URL == "" {
		writeError(w, http.StatusBadRequest, "async activation requires a callback")
		return
	}
	go h.activateAsync(poolID, act)
	writeJSON(w, http.StatusAccepted, map[string]string{"poolId": poolID, "status": pool.StateActivating})
}

// preferRespondAsync matches the Prefer header's respond-async token,
// case-insensitively (RFC 7240 preference tokens are case-insensitive).
// Combined forms ("respond-async, wait=10") are still not recognized — by
// design, mirroring the gateway's single-token match.
func preferRespondAsync(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Prefer"), "respond-async")
}

// activateAsync runs the activation detached and delivers the .result event.
// At-most-once: a crash mid-flight drops the callback (failure-semantics).
func (h *poolsHandler) activateAsync(poolID string, act *pool.Activation) {
	// The service applies the default TimeoutSeconds during Activate — after
	// this deadline is computed — so an omitted timeout must budget for the
	// default (300s), not zero.
	timeout := act.TimeoutSeconds
	if timeout <= 0 {
		timeout = 300
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout+120)*time.Second)
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
