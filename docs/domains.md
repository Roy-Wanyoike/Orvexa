# Domain Ownership Map

The dependency rule (ADR-0002): modules communicate through **published events**, **defined interfaces**, or the **HTTP API**. Importing another module's storage is forbidden.

| Module | Owns (tables) | Emits (topics) | Notes |
|---|---|---|---|
| `internal/tenancy` | organizations, tenants, api_keys | — | control plane; auth middleware; tenant always from key |
| `internal/platform/outbox` | outbox_events | — | the only sanctioned event write path |
| `internal/platform/bus` | — | — | in-proc driver; NATS driver = roadmap O-10 |
| `internal/customers` | customers, customer_identifiers, customer_tags | customer.created | normalization + resolution is the identity backbone |
| `internal/conversations` | conversations, conversation_participants | conversation.opened, conversation.closed | continuous customer relationship |
| `internal/interactions` | interactions | interaction.created/updated/completed | unified abstraction; guarded state machine; auto conversation find-or-open |
| `internal/telephony` | — (voice interactions) | via webhooks: call.* | VoiceProvider port; providers are adapters |
| `internal/messaging` | — (message interactions) | via webhooks: message.* | MessagingProvider port |
| `internal/comms` | provider_events (ledger) | — | simulator adapter + signed-webhook processor |
| `internal/agents` | agents, agent_skills | — | identity + skills (presence is routing's) |
| `internal/queues` | queues, queue_skills | — | configuration + read model |
| `internal/routing` | routing_decisions, agent_presence | routing.decision.recorded, agent.state.changed | pure engine + presence store |
| `internal/cases` | cases, case_notes, case_interactions | case.opened/updated/closed | independent from interactions |
| `internal/ai` | ai_agents, ai_agent_versions, ai_agent_tools, ai_agent_policies, ai_usage | ai.invocation, usage.recorded | gateway = only provider path |
| `internal/tools` | — | tool.execution.audited | the AI→side-effects boundary |
| `internal/workflows` | workflow_instances, workflow_steps | workflow.started/step.completed/completed/failed | durable, deterministic, traced |
| `internal/analytics` | interaction_fact, usage_fact | — | facts in, aggregations out |
| `internal/webhooks` | — (uses provider_events) | — | HMAC validation + dedup ingress |

**State machines (guarded, unit-tested):**

- Interaction: `pending → active → wrapup → completed | failed`, `pending → canceled | failed`. Terminal states sealed.
- Case: `open → in_progress → pending_customer → resolved → closed`, `resolved → in_progress` (reopen), `canceled` from any active state.
- Conversation: `open → closed` (terminal).
- Workflow: `running ↔ waiting_timer → completed | failed | canceled`.
- Presence: `available | busy | wrapup | offline`.
