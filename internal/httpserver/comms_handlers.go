package httpserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
)

// MountCalls wires the voice call resource. Call state is read through the
// interaction resource (GET /api/v1/interactions/{id}) — a call IS an
// interaction with channel=voice, so there is exactly one read model.
func MountCalls(r chi.Router, svc *telephony.Service) {
	if svc == nil {
		return
	}
	h := &callHandlers{svc: svc}
	r.Route("/calls", func(cr chi.Router) {
		cr.Post("/", h.place)
		cr.Post("/{id}/actions", h.action)
	})
}

type callHandlers struct{ svc *telephony.Service }

func (h *callHandlers) place(w http.ResponseWriter, r *http.Request) {
	var in telephony.PlaceCallInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	rec, err := h.svc.PlaceCall(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rec, nil)
}

func (h *callHandlers) action(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action      string `json:"action"`
		Destination string `json:"destination"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	rec, err := h.svc.ApplyAction(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), body.Action, body.Destination)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec, nil)
}

// MountMessages wires the messaging resource.
func MountMessages(r chi.Router, svc *messaging.Service) {
	if svc == nil {
		return
	}
	h := &messageHandlers{svc: svc}
	r.Route("/messages", func(mr chi.Router) {
		mr.Post("/", h.send)
	})
}

type messageHandlers struct{ svc *messaging.Service }

func (h *messageHandlers) send(w http.ResponseWriter, r *http.Request) {
	var in messaging.SendInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	rec, err := h.svc.Send(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rec, nil)
}
