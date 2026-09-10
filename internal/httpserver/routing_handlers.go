package httpserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/routing"
)

// MountRouting wires the routing read model + interaction routing action.
func MountRouting(r chi.Router, svc *routing.Service) {
	if svc == nil {
		return
	}
	r.Route("/routing", func(rr chi.Router) {
		rr.Post("/interactions/{id}", h_route(svc))
		rr.Get("/decisions", h_decisions(svc))
	})
	// presence lives with agents
	r.Put("/agents/{id}/presence", h_setPresence(svc))
	r.Get("/agents/{id}/presence", h_getPresence(svc))
}

func h_route(svc *routing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req routing.Request
		if err := decodeJSON(w, r, &req); err != nil {
			httpx.WriteError(w, err)
			return
		}
		req.InteractionID = chi.URLParam(r, "id")
		d, err := svc.Route(r.Context(), principal(r).TenantID, req)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, d, nil)
	}
}

func h_decisions(svc *routing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := pageFrom(r)
		items, cursor, err := svc.ListDecisions(r.Context(), principal(r).TenantID, r.URL.Query().Get("interaction_id"), page)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
	}
}

func h_setPresence(svc *routing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			httpx.WriteError(w, err)
			return
		}
		if err := svc.SetPresence(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), body.Status); err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": body.Status}, nil)
	}
}

func h_getPresence(svc *routing.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := svc.GetPresence(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, p, nil)
	}
}
