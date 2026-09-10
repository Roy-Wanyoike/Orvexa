package httpserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/agents"
	"github.com/Roy-Wanyoike/orvexa/internal/cases"
	"github.com/Roy-Wanyoike/orvexa/internal/conversations"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/queues"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
)

// DomainDeps bundles the services the v1 API exposes.
type DomainDeps struct {
	Customers     *customers.Service
	Conversations *conversations.Service
	Interactions  *interactions.Service
	Agents        *agents.Service
	Queues        *queues.Service
	Cases         *cases.Service
	Tenancy       *tenancy.Service
	Writer        *outbox.Writer
}

// MountV1 assembles the authenticated /api/v1 route tree. The tenant always
// derives from the API key; scopes gate action classes; per-principal token
// buckets bound abuse.
func MountV1(r chi.Router, deps DomainDeps, limiter *httpx.RateLimit, log httpx.Logger) {
	// Webhooks are public but signature-validated (wave O-4 mounts here).
	// Everything below requires an authenticated principal.
	auth := tenancy.AuthMiddleware(deps.Tenancy, "api", limiter, log)

	r.Route("/api/v1", func(v1 chi.Router) {
		v1.Use(auth)

		v1.Get("/", func(w http.ResponseWriter, _ *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"resources": []string{
					"customers", "conversations", "interactions",
					"agents", "queues", "cases",
				},
			}, nil)
		})

		MountCustomers(v1, deps.Customers)
		MountConversations(v1, deps.Conversations, deps.Interactions)
		MountInteractions(v1, deps.Interactions)
		MountAgents(v1, deps.Agents)
		MountQueues(v1, deps.Queues)
		MountCases(v1, deps.Cases)
	})
}
