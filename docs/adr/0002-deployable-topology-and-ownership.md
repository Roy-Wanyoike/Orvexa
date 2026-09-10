# ADR-0002 — Deployable topology and module ownership

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

ADR-0001 fixes the four-plane, event-driven model. This ADR pins the concrete topology: which binaries exist, what each owns, and the dependency rule between modules.

## Decision

### Deployables (cmd/)

| Binary | Plane | Owns | Scale signal |
|---|---|---|---|
| `api` | Interaction + Control | REST v1, auth, tenant resolution, webhook ingestion | request rate |
| `worker` | Execution + Intelligence | outbox dispatcher, consumers (audit, analytics, usage) | consumer lag |
| `realtime` | Interaction | WebSocket hub, fanout | connections, msg/s |
| `routing` *(reserved)* | Interaction | routing engine as scale-out service | decisions/s |
| `ai-worker` *(reserved)* | Intelligence | AI executions off the hot path | queue depth |
| `analytics-worker` *(reserved)* | Intelligence | facts pipeline | event volume |

Binaries marked *reserved* exist as processes when their wave lands; until then the capability runs inside `api`/`worker` and extraction is a config+deployment change, not a redesign.

### Module ownership map (internal/)

| Module | Tables it owns | May import |
|---|---|---|
| `platform/*` | — (infrastructure) | pkg/* |
| `tenancy` | organizations, tenants, api_keys | platform |
| `customers` | customers, customer_identifiers, tags, consents | platform |
| `conversations` | conversations, conversation_participants | platform, customers(events only) |
| `interactions` | interactions | platform, conversations(events only) |
| `telephony`, `messaging` | calls, messages | platform, interactions(events only) |
| `agents`, `queues` | agents, skills, queues, presence | platform |
| `routing` | routing_decisions | platform, agents(presence iface) |
| `cases` | cases, case_notes | platform |
| `ai` | ai_agents*, ai_usage | platform |
| `tools` | — (uses other modules' services via interfaces) | platform |
| `workflows` | workflow_* | platform (tools via gateway interface) |
| `analytics` | *_fact | platform (events only) |
| `audit` | audit_events | platform (events only) |

### Dependency rule

Modules communicate through (1) published domain events, (2) explicitly defined interfaces, or (3) the HTTP API. Importing another module's repository/storage is forbidden. `pkg/*` depends on nothing internal.

## Consequences

- No cross-module JOINs in application SQL; cross-domain reads go through events or APIs.
- Extraction of a module into its own service = move its package + tables; call sites already use interfaces/events.
- CI enforces build+vet+test for every deployable independently.

## Security implications

Tenant resolution and API-key auth live in `tenancy` + transport middleware; every module receives an authenticated, tenant-scoped context — no module may accept a raw tenant id from request bodies.

## Operational implications

Each deployable has independent health, shutdown and scaling; the outbox dispatcher is the only component with at-least-once delivery semantics, and all consumers are idempotent by contract.
