package httpserver

import (
	"io"
	"net/http"
	"time"

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
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
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
	Webhooks      *webhooks.Gateway
}

// MountV1 assembles the /api/v1 route tree.
//
// Public surface: POST /api/v1/webhooks/{provider} — signature-validated and
// IP-rate-limited, because providers cannot hold API keys. Everything else
// requires an authenticated principal: scopes gate action classes and the
// tenant ALWAYS derives from the API key, never from request bodies.
func MountV1(r chi.Router, deps DomainDeps, limiter *httpx.RateLimit, webhookLimiter *httpx.RateLimit, log httpx.Logger) {
	r.Route("/api/v1", func(v1 chi.Router) {
		// public ingress — hardened webhook gateway
		v1.Post("/webhooks/{provider}", webhookHandler(deps.Webhooks, webhookLimiter))

		auth := tenancy.AuthMiddleware(deps.Tenancy, "api", limiter, log)
		v1.Group(func(authed chi.Router) {
			authed.Use(auth)

			authed.Get("/", func(w http.ResponseWriter, _ *http.Request) {
				httpx.WriteJSON(w, http.StatusOK, map[string]any{
					"resources": []string{
						"customers", "conversations", "interactions",
						"agents", "queues", "cases",
					},
				}, nil)
			})

			MountCustomers(authed, deps.Customers)
			MountConversations(authed, deps.Conversations, deps.Interactions)
			MountInteractions(authed, deps.Interactions)
			MountAgents(authed, deps.Agents)
			MountQueues(authed, deps.Queues)
			MountCases(authed, deps.Cases)
		})
	})
}

// webhookHandler ingests provider webhooks: bounded body, signature validated
// inside the gateway, IP-rate-limited, no business logic in the handler.
func webhookHandler(g *webhooks.Gateway, limiter *httpx.RateLimit) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g == nil {
			httpx.WriteError(w, errWebhookUnavailable)
			return
		}
		if ok, retry := limiter.Allow(clientIP(r), time.Now()); !ok {
			w.Header().Set("Retry-After", retryAfterSeconds(retry))
			httpx.WriteError(w, errWebhookRateLimited)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, webhooks.MaxPayloadBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpx.WriteError(w, errWebhookBodyUnreadable)
			return
		}
		res, err := g.Ingest(r.Context(), chi.URLParam(r, "provider"), body, r.Header.Get(webhooks.SignatureHeader))
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		status := http.StatusOK
		if !res.Duplicate {
			status = http.StatusAccepted
		}
		httpx.WriteJSON(w, status, res, nil)
	}
}

func clientIP(r *http.Request) string {
	// direct socket address: the deployment must only expose the API via a
	// trusted proxy that overwrites X-Forwarded-For (runbook: operations.md)
	host := r.RemoteAddr
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			host = host[:i]
			break
		}
	}
	return host
}

func retryAfterSeconds(d time.Duration) string {
	s := int(d.Seconds())
	if s < 1 {
		s = 1
	}
	return itoa(s)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
