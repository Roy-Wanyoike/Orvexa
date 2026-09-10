package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Roy-Wanyoike/orvexa/internal/agents"
	"github.com/Roy-Wanyoike/orvexa/internal/cases"
	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/conversations"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/outbox"
	"github.com/Roy-Wanyoike/orvexa/internal/queues"
	"github.com/Roy-Wanyoike/orvexa/internal/routing"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	"github.com/Roy-Wanyoike/orvexa/internal/webhooks"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
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
	CommsProcessor *comms.Processor
	Calls         *telephony.Service
	Messages      *messaging.Service
	Routing       *routing.Service
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
		v1.Post("/webhooks/{provider}", webhookHandler(deps.Webhooks, deps.CommsProcessor, webhookLimiter))

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
			MountCalls(authed, deps.Calls)
			MountMessages(authed, deps.Messages)
			MountRouting(authed, deps.Routing)
		})
	})
}

// webhookHandler ingests provider webhooks: bounded body, signature validated
// inside the gateway, IP-rate-limited, no business logic in the handler.
func webhookHandler(g *webhooks.Gateway, processor *comms.Processor, limiter *httpx.RateLimit) http.HandlerFunc {
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
		// processing: the persisted ledger event is applied to the domain
		// immediately (crash between ingest and process is recoverable from
		// the provider_events ledger — reaper tracked in the infra wave).
		processed := true
		if processor != nil {
			var ev comms.ProviderEvent
			if err := json.Unmarshal(body, &ev); err == nil {
				if perr := processor.Process(r.Context(), &ev); perr != nil {
					// surfaced, never swallowed: carriers retry non-2xx;
					// gateway dedup keeps replays single-effect.
					httpx.WriteError(w, perr)
					return
				}
			}
		}
		httpx.WriteJSON(w, status, map[string]any{
			"event_id": res.EventID, "duplicate": res.Duplicate, "accepted": res.Accepted, "processed": processed,
		}, nil)
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
