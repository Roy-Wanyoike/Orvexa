package httpserver

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/analytics"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/workflows"
)

// MountWorkflows wires the execution-plane workflow endpoints.
func MountWorkflows(r chi.Router, e *workflows.Engine) {
	if e == nil {
		return
	}
	r.Route("/workflows", func(wr chi.Router) {
		wr.Post("/callbacks", h_startCallback(e))
		wr.Post("/collections", h_startCollections(e))
		wr.Get("/{id}", h_workflowGet(e))
		wr.Post("/{id}/cancel", h_workflowCancel(e))
	})
}

func h_startCallback(e *workflows.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in workflows.StartCallbackInput
		if err := decodeJSON(w, r, &in); err != nil {
			httpx.WriteError(w, err)
			return
		}
		inst, err := e.StartCallback(r.Context(), principal(r).TenantID, in)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, inst, nil)
	}
}

func h_startCollections(e *workflows.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in workflows.StartCollectionsInput
		if err := decodeJSON(w, r, &in); err != nil {
			httpx.WriteError(w, err)
			return
		}
		inst, err := e.StartCollections(r.Context(), principal(r).TenantID, in)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, inst, nil)
	}
}

func h_workflowGet(e *workflows.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, err := e.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, inst, nil)
	}
}

func h_workflowCancel(e *workflows.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, err := e.Cancel(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, inst, nil)
	}
}

// MountAnalytics wires the analytics read API.
func MountAnalytics(r chi.Router, s *analytics.Service) {
	if s == nil {
		return
	}
	r.Get("/analytics/summary", h_analyticsSummary(s))
}

func h_analyticsSummary(s *analytics.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		parseTime := func(name string) time.Time {
			if v := q.Get(name); v != "" {
				if t, err := time.Parse(time.RFC3339, v); err == nil {
					return t
				}
			}
			return time.Time{}
		}
		sum, err := s.Summary(r.Context(), principal(r).TenantID, parseTime("from"), parseTime("to"))
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, sum, nil)
	}
}
