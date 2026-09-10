# Domain Ownership Map

The dependency rule (ADR-0002): modules communicate through **published events**, **defined interfaces**, or the **HTTP API**. Importing another module's storage is forbidden.

| Module | Owns (tables) | Emits (topics) | Notes |
|---|---|---|---|
| `internal/tenancy` | organizations, tenants, api_keys | — | control plane; auth middleware; tenant always from key. The append-only `audit_events` trail (migration 0001) is control-plane and is written by the worker's audit consumer |
| `internal/platform/outbox` | outbox_events | — | the only sanctioned event write path |
| `internal/platform/bus` | — | — | in-proc driver + NATS JetStream driver (`nats.go`, same `Bus` interface); `cmd` wiring for the JetStream driver is tracked under roadmap [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10) |
| `internal/httpserver` | — (transport) | — | REST v1 route tree, middleware chain, typed error envelope; owns no tables |
| `internal/customers` | customers, customer_identifiers, customer_tags | customer.created | normalization + resolution is the identity backbone |
| `internal/conversations` | conversations, conversation_participants | conversation.opened, conversation.closed | continuous customer relationship |
| `internal/interactions` | interactions | interaction.created/updated/completed | unified abstraction; guarded state machine; auto conversation find-or-open |
| `internal/telephony` | — (voice interactions) | via webhooks: call.* | VoiceProvider port; providers are adapters |
| `internal/messaging` | — (message interactions) | via webhooks: message.* | MessagingProvider port |
| `internal/comms` | provider_events (ledger) | — | simulator adapter + signed-webhook processor; `registry` (provider names, credential env parsing, redaction) and `factory` (registry → adapters → `cmd/api` wiring) are config-only subpackages with no tables of their own |
| `internal/agents` | agents, agent_skills | — | identity + skills (presence is routing's) |
| `internal/queues` | queues, queue_skills | — | configuration + read model |
| `internal/routing` | routing_decisions, agent_presence | routing.decision.recorded, agent.state.changed | pure engine + presence store (in-process TTL or Redis cache driver) |
| `internal/cases` | cases, case_notes, case_interactions | case.opened/updated/closed | independent from interactions |
| `internal/ai` | ai_agents, ai_agent_versions, ai_agent_tools, ai_agent_policies, ai_usage | ai.invocation, usage.recorded | gateway = only provider path |
| `internal/tools` | — | tool.execution.audited | the AI→side-effects boundary |
| `internal/workflows` | workflow_instances, workflow_steps | workflow.started/step.completed/completed/failed | durable, deterministic, traced; optional `temporaldriver` (build tag `temporal`) implements the same lifecycle on a Temporal fleet ([ADR-0009](adr/0009-temporal-driver.md)) |
| `internal/analytics` | interaction_fact, usage_fact | — | facts in, aggregations out; `analytics/clickhouse` swaps the facts sink to a batched ClickHouse writer when `ORVEXA_CLICKHOUSE_URL` is set (Postgres path otherwise) |
| `internal/search` | — (OpenSearch read model) | — | consumes conversation/interaction events; projects the `orvexa-conversations` index; bounded queue sheds events under backpressure — search is never a dependency of the hot path |
| `internal/identity` | role_bindings, capability_catalog (migration 0012) | — | OIDC verification + capability RBAC ([docs/rbac.md](rbac.md), [ADR-0008](adr/0008-oidc-identity-capability-rbac.md)); the token asserts identity, the rows here decide authorization |
| `internal/webhooks` | — (uses provider_events) | — | HMAC validation + dedup ingress. `webhook_endpoints`/`webhook_deliveries` (migration 0006) are reserved outbound-delivery tables — no code writes them yet |

**State machines (guarded, unit-tested):**

- Interaction: `pending → active → wrapup → completed | failed`, `pending → canceled | failed`. Terminal states sealed.
- Case: `open → in_progress → pending_customer → resolved → closed`, `resolved → in_progress` (reopen), `canceled` from any active state.
- Conversation: `open → closed` (terminal).
- Workflow: `running ↔ waiting_timer → completed | failed | canceled`.
- Presence: `available | busy | wrapup | offline`.

**Capability model (authorization):**

OIDC principals are governed by a closed role → capability catalog. The IdP asserts identity; Orvexa owns authorization ([docs/rbac.md](rbac.md), [ADR-0008](adr/0008-oidc-identity-capability-rbac.md)):

| Role | Capabilities |
|---|---|
| `viewer` | `interaction.read`, `case.read`, `search.read` |
| `agent` | + `interaction.write`, `case.write` |
| `supervisor` | + `workflow.run` |
| `admin` | + `admin.org` |

- Enforcement points: `identity.RequireCapability` and `identity.RequireMethodCapability` wrap the conversations/interactions, cases and workflows mounts in the v1 route tree — GET/HEAD/OPTIONS demand the read capability, every other method the write capability.
- Resolution: with a database, `role_bindings` rows joined to `capability_catalog` are authoritative behind a short-TTL (30s default, bounded 4096-entry) resolver — a revocation takes effect within the TTL even while the token is still cryptographically valid. Without a database, the IdP-asserted `roles` claim maps through the embedded catalog (static posture, logged at boot). Unknown `(tenant, subject)` resolves to an empty binding → 403.
- API keys keep their minted scopes; the legacy blanket scopes (`api`, `*`) pass capability gates byte-identically, so existing integrations are unchanged.
- The closed role/capability sets live in three places that tests keep equal: `internal/identity/roles.go`, migration `0012_rbac.sql`, and the matrix in [docs/rbac.md](rbac.md).
