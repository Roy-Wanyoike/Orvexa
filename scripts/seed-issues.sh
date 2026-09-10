#!/usr/bin/env bash
# Creates the Orvexa wave issues. Idempotent: skips if an issue with the same title exists.
set -euo pipefail
: "${GH_TOKEN:?}"
G=/home/z/.local/bin/gh
R=Roy-Wanyoike/Orvexa

create() {
  local title="$1"; local labels="$2"; local body="$3"
  if $G issue list -R "$R" --state all --search "\"$title\" in:title" --json number --jq 'length' 2>/dev/null | grep -q '^1$'; then
    echo "skip: $title"; return
  fi
  $G issue create -R "$R" --title "$title" --label "$labels" --body "$body" >/dev/null && echo "created: $title"
}

ISSUE_TMPL='### Problem statement
%s

### Context
%s

### Business impact
%s

### Scope
%s

### Out of scope
%s

### Acceptance criteria
%s

### Dependencies
%s

### Security considerations
%s

### Testing requirements
%s

### Documentation requirements
%s

### Files/directories likely to change
%s

### API impact
%s

### Database impact
%s

### Event/schema impact
%s

### Observability requirements
%s

### Rollback considerations
%s

### Definition of Done
Implementation complete; acceptance criteria pass; unit tests pass; integration tests pass where applicable; security reviewed; docs updated; PR merged (never direct push); issue closed only when AC are satisfied.'

create "[O-1] Foundation: Go module, deployables, shared kernels, control-plane schema, CI" "area/backend,priority/p0,type/feature" "$(printf "$ISSUE_TMPL" \
'Repository is empty; the platform needs its module skeleton, four deployable entry points (api, worker, realtime + future routing/ai-worker/analytics-worker), shared pkg kernels, control-plane migrations and CI before any domain work.' \
'Modular distributed architecture per docs/adr/0001: strong domain boundaries inside ~8 deployables. Local-safe default profile (in-process bus, no external services) so the full loop is runnable and testable in CI without infra.' \
'Unblocks all subsequent waves; establishes quality gates (build, vet, test, race) enforced on every PR.' \
'go.mod; cmd/{api,worker,realtime}; pkg/{config,errors,logging,pagination,events,idempotency}; internal/platform/{db,httpx}; migrations/0001_control_plane.sql (organizations, tenants, api_keys, audit_events); .github/workflows/ci.yml; Makefile; ADRs 0001-0002.' \
'Domain modules (waves 2-5); NATS/Temporal adapters (tracked separately).' \
'go build ./... passes; go vet ./... clean; go test ./... green with -race; /healthz and /readyz live on api; migrations apply in order; CI runs on every PR and blocks merge when red.' \
'None (first issue).' \
'API key table stores SHA-256 hashes only; no secrets committed; security headers + request-id + recovery middleware from day one; /healthz and /readyz unauthenticated by design, everything else authenticated.' \
'Unit tests: config parsing, errors mapping, pagination, idempotency key derivation, event envelope validation. CI: build+vet+test+race.' \
'README quickstart; .env.example; ADR-0001 (architecture), ADR-0002 (deployable topology).' \
'go.mod, cmd/**, pkg/**, internal/platform/**, migrations/0001_control_plane.sql, .github/workflows/ci.yml, Makefile, docs/adr/000{1,2}*.md' \
'GET /healthz, GET /readyz only.' \
'Creates control-plane tables: organizations, tenants, api_keys (hashed), audit_events (append-only).' \
'None yet (envelope + topic registry defined in pkg/events).' \
'Request logs carry request_id; health checks report db status; audit_events schema ready.' \
'Migrations are forward-only SQL; rollback = drop schema; no data at risk at this stage.' \
'Code merged via PR; CI green; docs synced.')"

create "[O-2] Domain core: customers, conversations, interactions, agents, queues, cases + REST v1 + tenant auth" "area/backend,priority/p0,type/feature" "$(printf "$ISSUE_TMPL" \
'The interaction plane needs its core domain model: Customer→Conversation→Interaction→Case with multi-identifier customer resolution, unified interaction abstraction across channels, agent/queue structures, and a REST v1 surface — all under tenant resolution + API-key auth.' \
'Domain architecture doc §§4-8, 25; API architecture §28. A phone call is an interaction inside a conversation; cases are independent of interactions.' \
'Core product capability; every later feature (routing, AI, workflows, analytics) consumes these models and events.' \
'Migrations 0002-0005; internal/{tenancy,customers,conversations,interactions,agents,queues,cases} services+repos; REST resources under /api/v1/{customers,conversations,interactions,agents,queues,cases}; cursor pagination; state machines with guarded transitions.' \
'Provider adapters, routing engine, AI runtime (later waves).' \
'Identifier resolution returns same customer regardless of channel; interaction lifecycle guards reject illegal transitions (409); conversation participants preserve one continuous context across channels; every list endpoint paginates and bounds page size; all mutation endpoints authenticated + audit-logged.' \
'O-1.' \
'API keys hashed (SHA-256, constant-time compare); tenant derived from key, never from body; cross-tenant access by ID returns 404; validation on every input.' \
'Unit: state machines (all transition pairs), identifier resolution, pagination encoding. Integration (skip w/o TEST_DATABASE_URL): CRUD + lifecycle + cross-tenant isolation.' \
'API reference seed in api/openapi/orvexa-v1.yaml; docs/domains.md entity map.' \
'migrations/000{2,3,4,5}*.sql, internal/{tenancy,customers,conversations,interactions,agents,queues,cases}/**, internal/httpserver/**' \
'Full v1 resource surface (see OpenAPI).' \
'customers, customer_identifiers, conversations, conversation_participants, interactions, agents, agent_skills, queues, cases, case_notes tables with FK indexes.' \
'interaction.created/updated, conversation.opened/closed, case.opened/closed, agent.state.changed envelopes emitted to outbox.' \
'Structured logs with tenant_id + request_id on every handler; audit_events for mutations.' \
'Forward-only migrations; services are stateless so rollback is code revert.' \
'PR merged; AC verified by tests; docs synced.')"

create "[O-3] Event backbone: transactional outbox, dispatcher, bus abstraction (inproc + NATS), webhook ingestion gateway" "area/backend,priority/p0,type/feature" "$(printf "$ISSUE_TMPL" \
'Domains must communicate via events without distributed transactions, and external providers must enter through a hardened webhook layer — otherwise DB/state drift and webhook replay/forgery become structural risks.' \
'Architecture doc §§17-19, 27. Transactional outbox (BEGIN write + outbox insert COMMIT; dispatcher publishes). Webhook gateway: authn, signature validation, dedup, rate limit, persistence — no business logic in handlers.' \
'Decouples every subsystem; prevents lost events and duplicate side-effects (double SMS/charge/case).' \
'pkg/events envelope+topics finalized; internal/platform/bus (Bus interface, inproc driver, NATS JetStream driver behind config); internal/platform/outbox (writer+dispatcher with retry/backoff); internal/webhooks (HMAC sig validation, provider_event_id dedup with unique constraint, rate limit); migrations 0006.' \
'O-1, O-2.' \
'No lost events on crash between DB write and publish (dispatcher recovery); duplicate webhook delivery is idempotent (unique provider_event_id); invalid signatures rejected 401 without processing; outbox rows carry correlation ids.' \
'O-2.' \
'HMAC-SHA256 signature validation with timing-safe compare; dedup by (provider, provider_event_id) unique; webhook bodies size-capped; no secrets logged.' \
'Unit: envelope validation, outbox claim/retry semantics, signature validation (tampered/replayed/valid). Integration: dispatcher drains outbox to bus; duplicate webhook is single-effect.' \
'docs/architecture.md event section updated; ADR-0003 (event delivery + outbox).' \
'pkg/events/**, internal/platform/{bus,outbox}/**, internal/webhooks/**, migrations/0006_events_infra.sql, cmd/worker/**' \
'POST /api/v1/webhooks/{provider} (public, signature-validated); everything else unchanged.' \
'outbox_events, webhook_deliveries, provider_events tables.' \
'This issue DEFINES the envelope + topic registry consumed by all later waves.' \
'Dispatcher logs lag + failures; metrics counters for published/consumed/failed; webhook gateway logs rejections by reason.' \
'Dispatcher is idempotent (at-least-once, consumers dedupe by event id); bus driver switch is config-only.' \
'PR merged; tests green; ADR merged.')"

create "[O-4] Communications: provider-agnostic telephony + messaging ports, simulator adapter, interaction lifecycle wiring" "area/backend,priority/p0,type/feature" "$(printf "$ISSUE_TMPL" \
'Voice/messaging must sit behind hexagonal ports so the core never knows the carrier, and the platform needs a working end-to-end channel loop (create call → ringing → connected → completed; send WhatsApp → delivered → replied) that re-enters through the webhook gateway like a real provider.' \
'Architecture doc §§8-9, 26. Provider adapters (twilio/africastalking/whatsapp_cloud) are separate tracked issues; this wave ships ports + a faithful simulator.' \
'The product demonstrably places calls and exchanges messages end-to-end today, and real carriers are additive adapters only.' \
'internal/telephony (VoiceProvider port, call model, service); internal/messaging (MessagingProvider ports, message model, service); adapters/simulator for both (stateful lifecycle progression publishing signed webhooks into the gateway); REST: POST /calls, POST /calls/{id}/actions, POST /messages; interactions auto-created per communication.' \
'O-2, O-3.' \
'Interaction records carry channel/direction/status/source/destination; every state change is event-sourced through the outbox; simulator signs webhooks with the platform secret and is explicitly labelled SIMULATOR provenance in payloads.' \
'O-3.' \
'Simulator never dials external networks; provider credentials live only in env; webhook route validates HMAC before parse.' \
'Unit: port contracts, simulator lifecycle transitions, interaction auto-creation. Integration: full loop create→webhook→interaction→events.' \
'OpenAPI updated; ADR-0004 (provider-agnostic comms + adapter roadmap).' \
'internal/telephony/**, internal/messaging/**, internal/comms/simulator/**, cmd/api routes' \
'POST /api/v1/calls, POST /api/v1/calls/{id}/actions, POST /api/v1/messages, POST /api/v1/webhooks/{provider}.' \
'Provider-neutral call/message columns; interaction channel enum extended.' \
'call.requested/ringing/connected/completed/failed, message.sent/delivered/read/received envelopes.' \
'Provider latency + failure metrics; simulator transitions audited.' \
'Simulator is default and revertible via config; ports unchanged by future adapters.' \
'PR merged; E2E loop demo script passes; docs updated.')"

create "[O-5] Routing engine + agent presence + audit consumer" "area/backend,priority/p1,type/feature" "$(printf "$ISSUE_TMPL" \
'Interactions need deterministic, recorded routing decisions (skills, language, priority, business hours, availability, SLA) and agents need presence separated from identity; consequential actions need a durable audit trail consumer.' \
'Architecture doc §§10-11, 31. Every routing decision recorded; presence in a fast store with reconstructable source of truth; audit consumer subscribes to domain events.' \
'Customers reach the right agent; supervisors get accountability; the critical path stays lean.' \
'internal/routing (decision engine + recorded decisions + assignment); internal/agents presence store (interface + inproc default, PG source of truth); internal/audit consumer; REST: /routing/decisions read model, agent presence endpoints; migrations 0007.' \
'O-2, O-3.' \
'Routing decisions are immutable records (inputs, candidate ranking, outcome, latency); presence transitions event-sourced; audit consumer writes append-only rows with actor/action/resource/before/after/correlation id.' \
'O-2, O-3.' \
'Presence endpoints authenticated; routing engine has no PII in logs beyond ids.' \
'Unit: decision matrix (priority/skills/hours/availability), presence transitions. Integration: inbound interaction → decision → assignment → agent.state.changed.' \
'OpenAPI + docs updated; decision record schema documented.' \
'internal/routing/**, internal/agents/presence*, internal/audit/**, migrations/0007_routing_audit.sql, cmd/worker' \
'GET /api/v1/routing/decisions, GET/PUT /api/v1/agents/{id}/presence.' \
'routing_decisions, agent_presence tables.' \
'routing.decision.recorded, agent.available/busy/wrapup/offline topics formalized.' \
'Decision latency metric; audit consumer lag metric.' \
'Engine is pure over inputs; config changes revert safely.' \
'PR merged; tests green; docs synced.')"

create "[O-6] Realtime gateway: WebSocket hub over the bus with auth, caps and presence-aware fanout" "area/backend,priority/p1,type/feature" "$(printf "$ISSUE_TMPL" \
'Agent applications need realtime events (call.ringing, agent.assigned, ai.suggestion, queue.updated) — polling Postgres is not acceptable; the gateway must subscribe to the bus and fan out per-tenant/per-agent with authentication and connection caps.' \
'Architecture doc §12.' \
'Agent desktop UX is realtime; unauthenticated or uncapped sockets would be a DoS and data-leak surface.' \
'cmd/realtime + internal/realtime: WS hub, per-tenant channels + per-agent topics, auth on upgrade (API key), origin check, ping/pong keepalive, connection caps per principal and globally, event filtering by subscription.' \
'O-3, O-5.' \
'Sockets only receive events for the authenticated principal tenant; caps enforced (default 10/principal, 10k global, configurable); slow consumers dropped, not blocking publishers.' \
'O-3, O-5.' \
'Origin allowlist; auth before upgrade completes; no PII in URL query strings (token via header).' \
'Unit: subscription filtering, cap enforcement. Integration (inproc bus): publish event → subscribed client receives; foreign tenant does not.' \
'OpenAPI realtime section; runbook notes.' \
'internal/realtime/**, cmd/realtime/**' \
'GET /realtime (websocket upgrade) — separate port from API.' \
'None (consumes bus).' \
'Reuses envelope registry; adds realtime.session.opened/closed local metrics only.' \
'Active connections gauge, dropped-slow-consumer counter.' \
'Gateway is stateless besides socket registry; revert = disable deployment.' \
'PR merged; integration test shows tenant isolation; docs updated.')"

create "[O-7] Intelligence: AI gateway, AI agent config/runtime, tool gateway security boundary" "area/backend,priority/p1,type/feature" "$(printf "$ISSUE_TMPL" \
'AI must be a governed subsystem: provider/model routing, usage+cost metering, timeouts/retries, and a Tool Gateway that is the ONLY path from AI to side-effects — AI never holds database, cloud, payment or CRM credentials.' \
'Architecture doc §§13-15.' \
'Enables AI agents (suggestions, summaries, collections) without creating an unguarded route to customer data or actions.' \
'internal/ai/gateway (provider interface, rules-based default provider, model router config, token/cost metering → usage.recorded events, timeout+retry caps); internal/ai/runtime (agents/versions/tools/policies config, context assembly); internal/tools (gateway: authorization, tenant validation, schema validation, rate limit, audit, execution) with first tools: get_customer, create_case, send_whatsapp; migrations 0008.' \
'O-2, O-3.' \
'Tool executions audited with actor=ai_agent:<id>; schema-validated inputs only; per-tenant tool allowlists; AI outputs never auto-executed without policy; provider keys env-only, never logged.' \
'O-2, O-3.' \
'Unit: gateway metering/retry/timeout, tool policy matrix, schema validation. Integration: ai.suggestion flow uses tools through the gateway and writes usage.recorded.' \
'OpenAPI (ai endpoints), ADR-0005 (tool gateway boundary), docs/ai.md.' \
'internal/ai/**, internal/tools/**, migrations/0008_ai.sql' \
'POST /api/v1/ai/agents/{id}/invoke (internal), GET /api/v1/ai/agents; tool exec is service-internal only (no public route).' \
'ai_agents, ai_agent_versions, ai_agent_tools, ai_agent_policies, ai_usage tables.' \
'ai.invocation.completed, usage.recorded, tool.execution.audited topics.' \
'Token usage + latency + cost per invocation; tool gateway decisions logged.' \
'rules provider is default (no external calls); gateway config revertible.' \
'PR merged; security review notes in PR body; docs updated.')"

create "[O-8] Execution: durable workflow engine (callback + collections) and analytics worker + facts API" "area/backend,priority/p1,type/feature" "$(printf "$ISSUE_TMPL" \
'Long-running processes (callbacks, collections ladders) must survive restarts, and dashboard aggregations must never run against transactional Postgres — facts must flow through events into an analytics store with a read API.' \
'Architecture doc §§16, 22, 32.' \
'Callbacks/collections become reliable products; supervisors get fast analytics without OLTP impact.' \
'internal/workflows (definition model, durable DB-backed executor with timer tasks — deterministic steps, retries with backoff, crash-safe resume; CreateCallbackWorkflow + CollectionsWorkflow implemented); internal/analytics (event consumer → interaction_fact/agent_activity_fact/queue_fact/ai_interaction_fact; read API with fixed aggregations); usage metering consumer; migrations 0009-0010.' \
'O-3, O-4, O-7.' \
'Workflow state transitions audited; workflows cannot execute tools directly (they go through the tool gateway); analytics API is read-only and tenant-scoped; usage.recorded enables subscription+usage billing later.' \
'O-3.' \
'Unit: workflow step machine, timer scheduling/recovery, fact builders. Integration: callback workflow schedules→completes across simulated restart; analytics reflect emitted events.' \
'OpenAPI analytics section; ADR-0006 (workflow engine: local durable now, Temporal adapter path).' \
'internal/workflows/**, internal/analytics/**, migrations/000{9,10}*.sql, cmd/worker' \
'GET /api/v1/analytics/{interactions,agents,queues}; POST /api/v1/workflows/callbacks.' \
'workflow_instances, workflow_steps, workflow_timers, *_fact tables.' \
'workflow.started/step.completed/completed/failed, facts built from existing topics.' \
'Workflow lag/retry metrics; consumer lag; fact freshness timestamp on API.' \
'Executor is crash-safe by design (state in DB); Temporal extraction is config/adapter-level later.' \
'PR merged; integration tests green; docs+ADRs synced.')"

create "[O-9] Release gate: OpenAPI completeness, E2E demo script, docs tree, runbooks, QA report" "area/qa,priority/p0,type/feature" "$(printf "$ISSUE_TMPL" \
'Before customer onboarding the project needs a complete API contract artifact, a reproducible end-to-end demo, operational runbooks, an honest QA report with evidence, and a roadmap issue for every deferred production item (real carriers, NATS/Temporal/ClickHouse/OpenSearch/Redis adapters, OIDC, RBAC hardening).' \
'Master engineering protocol: no orphaned work; evidence-based readiness.' \
'Converts a codebase into an operable product.' \
'api/openapi/orvexa-v1.yaml complete; scripts/e2e-demo.sh (onboard → key → customer → inbound WhatsApp → routing → assignment → AI suggestion → case → callback workflow → analytics → audit); docs/{architecture,domains,runbooks/*}; qa/QA_REPORT.md with build/vet/test/race evidence and limitation ledger.' \
'O-1..O-8.' \
'E2E script must pass with zero external dependencies; every deferred item has a tracked issue; no secrets in any artifact.' \
'All waves.' \
'E2E script reviewed for injection/abuse surfaces; report states auth posture honestly.' \
'E2E script executed with transcript captured in QA report; go test -race ./... green; coverage summary recorded.' \
'All docs above; README status sync.' \
'api/openapi/**, docs/**, qa/**, scripts/**' \
'OpenAPI must match implemented routes exactly (verified by route-list check).' \
'None (documents existing migrations).' \
'Documents full topic registry.' \
'QA report includes metrics evidence (health, consumer lag, connection caps).' \
'N/A (documentation).' \
'PR merged; all wave issues closed; QA report verdict stated.')"

create "[O-10] Roadmap: production adapters (carriers, NATS, Temporal, ClickHouse, OpenSearch, Redis) + OIDC/RBAC" "area/infrastructure,priority/p2,type/feature" "$(printf "$ISSUE_TMPL" \
'The local-safe default profile uses in-process bus, DB-backed workflows and simulator providers. Production requires the real adapters — each behind the already-defined interface, each independently shippable.' \
'ADR-0001/0003/0004/0006 define the extraction paths.' \
'Turns the platform into a multi-tenant production deployment.' \
'Separate issues filed per adapter (twilio, whatsapp_cloud, africastalking, nats-jetstream driver hardening, temporal executor, clickhouse facts store, opensearch indexer, redis presence); this issue is the umbrella.' \
'O-9.' \
'Each adapter: credentials env-only, health checks, graceful degradation per architecture doc §33.' \
'Per-adapter tests + contract tests against ports.' \
'Per-adapter runbooks.' \
'adapters under internal/*/adapters/**, config' \
'None (same contracts).' \
'Per-adapter migrations only if state is required.' \
'None (same envelope).' \
'Per-adapter dashboards.' \
'Config-switch per adapter; simulators remain for CI.' \
'Umbrella tracks sub-issues; closed only when all adapters ship or are re-scoped.')"

echo "ALL ISSUES PROCESSED"