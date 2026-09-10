# Orvexa

**The AI-native Contact Center OS.** Orvexa is customer-interaction infrastructure where humans, AI agents, communication channels and business workflows operate as one system — built event-driven, workflow-driven, provider-agnostic and domain-oriented.

```text
CUSTOMER
   │
  Voice · WhatsApp · SMS · Email · Chat · USSD
   │
   ▼
INTERACTION PLANE ── customers, conversations, interactions, routing, agents, queues
   │ domain events (transactional outbox → JetStream-shaped bus)
   ▼
INTELLIGENCE PLANE              EXECUTION PLANE              CONTROL PLANE
AI gateway · AI agents          Workflows · Tool gateway     Tenancy · Identity · RBAC
Transcription · RAG · QA        Integrations · Payments      Billing/metering · Audit
Analytics (facts store)         Notifications · Webhooks     Configuration · Policies
```

## The four architectural boundaries

| Plane | Owns | Critical path? |
|---|---|---|
| **Interaction** | Realtime customer interactions: telephony, messaging, routing, agent assignment | **Yes** — nothing else may block a call |
| **Intelligence** | AI gateway, agents, transcription, analytics | No — consumes events, fails independently |
| **Execution** | Workflows, tool gateway, integrations, notifications | No — event-driven consumers |
| **Control** | Tenancy, identity, authorization, metering, audit | Yes — but only as fast checks |

Core domain model: **Customer → Conversation → Interaction → Case → Workflow**. A phone call is *not* the customer relationship; it is one interaction inside a continuous conversation.

## Repository layout

```text
cmd/            deployables: api, worker, realtime, routing, ai-worker, analytics-worker
internal/       domain modules (bounded contexts, dependency-rule enforced)
pkg/            shared kernels: events, errors, idempotency, pagination, logging
migrations/     ordered, reversible-where-practical SQL migrations
api/openapi/    API contracts (source of truth for v1)
docs/           architecture, ADRs, runbooks
qa/             release gate reports
```

## Status

**GO for pilot onboarding (sandbox/demo posture)** — see [qa/QA_REPORT.md](qa/QA_REPORT.md) for the evidence-backed verdict (build/vet/race-test matrix across 16 packages, boot verification, security posture) and the honest limitation ledger.

Waves shipped (each via issue → PR → merge): foundation, domain core, event backbone, communications, routing + presence, realtime gateway, AI + tool gateway, durable workflows + analytics.

Production adapters (real carriers, NATS, ClickHouse, OpenSearch, Redis, Temporal, OIDC/RBAC) are behind defined interfaces and tracked in the [roadmap issue](../../issues/10) — the full loop runs today with zero external dependencies.

## Quickstart

```bash
go build ./... && go test ./...
# no external services required for the default in-process profile
go run ./cmd/api          # HTTP API + :8080
go run ./cmd/worker       # outbox dispatcher + event consumers
go run ./cmd/realtime     # websocket gateway :8081
```

See [docs/runbooks/operations.md](docs/runbooks/operations.md) for configuration and deployment.

## License

MIT — see [LICENSE](LICENSE).
