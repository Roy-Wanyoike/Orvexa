package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/agents"
	"github.com/Roy-Wanyoike/orvexa/internal/cases"
	"github.com/Roy-Wanyoike/orvexa/internal/conversations"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/queues"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// ---- shared plumbing ----

const maxBodyBytes = 1 << 20 // 1 MiB hard cap on request bodies

// decodeJSON parses a bounded JSON body into dst (rejects empty/malformed).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return apperrors.Invalid("request.body_too_large", "request body exceeds 1 MiB")
		case errors.Is(err, io.EOF):
			return apperrors.Invalid("request.body_required", "request body is required")
		default:
			return apperrors.Invalid("request.body_malformed", "request body is not valid JSON for this resource")
		}
	}
	return nil
}

func principal(r *http.Request) *tenancy.Principal {
	p, _ := tenancy.PrincipalFrom(r.Context())
	return p // AuthMiddleware guarantees presence inside mounted trees
}

func pageFrom(r *http.Request) pagination.Page {
	q := r.URL.Query()
	p, err := pagination.Parse(q.Get("cursor"), atoi(q.Get("limit")))
	if err != nil {
		// malformed cursor → empty page with error surfaced by caller? keep simple:
		return pagination.Page{Limit: pagination.DefaultLimit}
	}
	return p
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ---- customers ----

// MountCustomers wires the customer resource.
func MountCustomers(r chi.Router, svc *customers.Service) {
	h := &customerHandlers{svc: svc}
	r.Route("/customers", func(cr chi.Router) {
		cr.Post("/", h.create)
		cr.Get("/", h.list)
		cr.Get("/{id}", h.get)
		cr.Post("/resolve", h.resolve)
	})
}

type customerHandlers struct{ svc *customers.Service }

func (h *customerHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in customers.CreateInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	c, err := h.svc.Create(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c, nil)
}

func (h *customerHandlers) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

func (h *customerHandlers) list(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.svc.List(r.Context(), principal(r).TenantID, page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

func (h *customerHandlers) resolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	c, err := h.svc.Resolve(r.Context(), principal(r).TenantID, body.Type, body.Value)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

// ---- conversations ----

// MountConversations wires the conversation resource.
func MountConversations(r chi.Router, conv *conversations.Service, inter *interactions.Service) {
	h := &conversationHandlers{conv: conv, inter: inter}
	r.Route("/conversations", func(cr chi.Router) {
		cr.Get("/", h.list)
		cr.Get("/{id}", h.get)
		cr.Post("/{id}/close", h.close)
		cr.Post("/{id}/assign", h.assign)
		cr.Get("/{id}/interactions", h.interactions)
	})
}

type conversationHandlers struct {
	conv *conversations.Service
	inter *interactions.Service
}

func (h *conversationHandlers) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.conv.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

func (h *conversationHandlers) list(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.conv.List(r.Context(), principal(r).TenantID, r.URL.Query().Get("status"), page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

func (h *conversationHandlers) close(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			httpx.WriteError(w, err)
			return
		}
	}
	c, err := h.conv.Close(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), body.Reason)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

func (h *conversationHandlers) assign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID string `json:"agent_id"`
		QueueID string `json:"queue_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	c, err := h.conv.Assign(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), body.AgentID, body.QueueID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

func (h *conversationHandlers) interactions(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.inter.ListByConversation(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

// ---- interactions ----

// MountInteractions wires the interaction resource.
func MountInteractions(r chi.Router, svc *interactions.Service) {
	h := &interactionHandlers{svc: svc}
	r.Route("/interactions", func(ir chi.Router) {
		ir.Post("/", h.create)
		ir.Get("/{id}", h.get)
		ir.Post("/{id}/transition", h.transition)
		ir.Post("/{id}/assign", h.assign)
	})
}

type interactionHandlers struct{ svc *interactions.Service }

func (h *interactionHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in interactions.CreateInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	rec, err := h.svc.Create(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rec, nil)
}

func (h *interactionHandlers) get(w http.ResponseWriter, r *http.Request) {
	rec, err := h.svc.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec, nil)
}

func (h *interactionHandlers) transition(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To        string `json:"to"`
		EndReason string `json:"end_reason"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	rec, err := h.svc.Transition(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"),
		interactions.Status(body.To), body.EndReason)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec, nil)
}

func (h *interactionHandlers) assign(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID string `json:"agent_id"`
		QueueID string `json:"queue_id"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	rec, err := h.svc.Assign(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), body.AgentID, body.QueueID)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec, nil)
}

// ---- agents ----

// MountAgents wires the agent resource.
func MountAgents(r chi.Router, svc *agents.Service) {
	h := &agentHandlers{svc: svc}
	r.Route("/agents", func(ar chi.Router) {
		ar.Post("/", h.create)
		ar.Get("/", h.list)
		ar.Get("/{id}", h.get)
	})
}

type agentHandlers struct{ svc *agents.Service }

func (h *agentHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in agents.CreateInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	a, err := h.svc.Create(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a, nil)
}

func (h *agentHandlers) get(w http.ResponseWriter, r *http.Request) {
	a, err := h.svc.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a, nil)
}

func (h *agentHandlers) list(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.svc.List(r.Context(), principal(r).TenantID, page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

// ---- queues ----

// MountQueues wires the queue resource.
func MountQueues(r chi.Router, svc *queues.Service) {
	h := &queueHandlers{svc: svc}
	r.Route("/queues", func(qr chi.Router) {
		qr.Post("/", h.create)
		qr.Get("/", h.list)
		qr.Get("/{id}", h.get)
	})
}

type queueHandlers struct{ svc *queues.Service }

func (h *queueHandlers) create(w http.ResponseWriter, r *http.Request) {
	var in queues.CreateInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	q, err := h.svc.Create(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, q, nil)
}

func (h *queueHandlers) get(w http.ResponseWriter, r *http.Request) {
	q, err := h.svc.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q, nil)
}

func (h *queueHandlers) list(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.svc.List(r.Context(), principal(r).TenantID, page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

// ---- cases ----

// MountCases wires the case resource.
func MountCases(r chi.Router, svc *cases.Service) {
	h := &caseHandlers{svc: svc}
	r.Route("/cases", func(cr chi.Router) {
		cr.Post("/", h.open)
		cr.Get("/", h.list)
		cr.Get("/{id}", h.get)
		cr.Post("/{id}/transition", h.transition)
		cr.Post("/{id}/notes", h.addNote)
		cr.Get("/{id}/notes", h.listNotes)
		cr.Post("/{id}/interactions/{interactionId}/link", h.link)
	})
}

type caseHandlers struct{ svc *cases.Service }

func (h *caseHandlers) open(w http.ResponseWriter, r *http.Request) {
	var in cases.CreateInput
	if err := decodeJSON(w, r, &in); err != nil {
		httpx.WriteError(w, err)
		return
	}
	c, err := h.svc.Open(r.Context(), principal(r).TenantID, in)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c, nil)
}

func (h *caseHandlers) get(w http.ResponseWriter, r *http.Request) {
	c, err := h.svc.Get(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

func (h *caseHandlers) list(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.svc.List(r.Context(), principal(r).TenantID, page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

func (h *caseHandlers) transition(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	c, err := h.svc.Transition(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"),
		cases.Status(body.To), body.Reason)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c, nil)
}

func (h *caseHandlers) addNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AuthorType string `json:"author_type"`
		AuthorID   string `json:"author_id"`
		Body       string `json:"body"`
		Internal   *bool  `json:"internal"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, err)
		return
	}
	internal := true
	if body.Internal != nil {
		internal = *body.Internal
	}
	n, err := h.svc.AddNote(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"),
		body.AuthorType, body.AuthorID, body.Body, internal)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, n, nil)
}

func (h *caseHandlers) listNotes(w http.ResponseWriter, r *http.Request) {
	page := pageFrom(r)
	items, cursor, err := h.svc.ListNotes(r.Context(), principal(r).TenantID, chi.URLParam(r, "id"), page)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items, map[string]string{"next_cursor": cursor})
}

func (h *caseHandlers) link(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.LinkInteraction(r.Context(), principal(r).TenantID,
		chi.URLParam(r, "id"), chi.URLParam(r, "interactionId")); err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil, nil)
}
