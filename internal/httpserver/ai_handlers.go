package httpserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/ai"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/tools"
)

// MountAI wires the intelligence plane endpoints.
func MountAI(r chi.Router, rt *ai.Runtime) {
	if rt == nil {
		return
	}
	r.Route("/ai", func(ar chi.Router) {
		ar.Get("/agents/{id}", h_aiAgent(rt))
		ar.Post("/agents/{id}/invoke", h_aiInvoke(rt))
		ar.Post("/agents/{id}/tools", h_aiTool(rt))
	})
}

func h_aiAgent(rt *ai.Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ag, err := rt.ResolveAgent(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, ag, nil)
	}
}

func h_aiInvoke(rt *ai.Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in ai.InvokeInput
		if err := decodeJSON(w, r, &in); err != nil {
			httpx.WriteError(w, err)
			return
		}
		res, err := rt.Invoke(r.Context(), principal(r).TenantID, in)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, res, nil)
	}
}

func h_aiTool(rt *ai.Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var c tools.Call
		if err := decodeJSON(w, r, &c); err != nil {
			httpx.WriteError(w, err)
			return
		}
		out, err := rt.InvokeTool(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), &c)
		if err != nil {
			// policy refusals still return the outcome envelope with the error
			if out != nil {
				httpx.WriteJSON(w, statusOf(err), out, nil)
				return
			}
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, out, nil)
	}
}
