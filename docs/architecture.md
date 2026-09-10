# Orvexa Architecture

The authoritative decisions live in the [ADRs](adr/). This document is the map.

## The four planes

```text
                 ┌───────────────────────────┐
                 │   Clients · Providers     │
                 └─────────────┬─────────────┘
                               │
      API (dual auth: OIDC Bearer | API key, tenancy, rate limits, webhooks)
                               │
        ┌──────────────────────┼──────────────────────┐
        ▼                      ▼                      ▼
  INTERACTION PLANE     EXECUTION PLANE        INTELLIGENCE PLANE
  customers             workflows              ai gateway + runtime
  conversations         (callback,             tool gateway (AI→actions)
  interactions           collections,          analytics facts (Postgres
  telephony/messaging    optional Temporal      or ClickHouse sink)
  routing + presence     driver)               conversation search
        │                      │                 (OpenSearch read model,
        │                      │                  sheds under load)
        │                      └──────────┬───────────┘
        ▼                                 ▼
        └──────────► EVENT BUS (outbox → at-least-once) ◄──────────┘
            InProc (default) · NATS JetStream driver behind the same Bus
                               │
                      CONTROL PLANE (everywhere)
      tenancy · api keys · OIDC identity + capability RBAC · audit · usage
```

## Critical path rule

Nothing outside telephony → routing → agent may block an interaction. AI, analytics, workflows, webhooks-out and audit are all asynchronous event consumers or bounded, fast control-plane checks.

## Domain model

**Customer → Conversation → Interaction → Case → Workflow**

- A phone call is **not** the customer relationship — it is one interaction inside a conversation. WhatsApp → voice → email on the same conversation preserves one continuous context.
- Customer identity is multi-identifier: `(tenant, type, value)` unique after normalization (phone→E.164 digits, email→lowercase), so a channel identity can never fork into two customers.
- Cases are independent of interactions; interactions link to cases.
- Workflows are long-running, durable, step-traced processes.

## Event backbone

Domains commit state **and** their events in one SQL transaction (`outbox.Writer.Insert(ctx, tx, env)`). The worker dispatcher claims batches with `FOR UPDATE SKIP LOCKED`, publishes to the bus, acks. Delivery is **at-least-once**; consumers dedupe by `Envelope.ID`. Topics come from a closed registry in `pkg/events`. Details: [ADR-0004](adr/0004-event-delivery-and-outbox.md).

The bus has two drivers behind one `platform/bus.Bus` interface: the in-process default and a NATS JetStream driver (`internal/platform/bus/nats.go` — synchronous bounded publishes, durable pull consumers per subscription, bounded NAK backoff, undecodable messages terminated). The JetStream driver ships code-complete and integration-tested; `cmd` process wiring still boots the in-process bus and says so (`ORVEXA_BUS_DRIVER=nats` currently logs the fallback in `cmd/worker`) — flipping the binaries to the driver is the remaining glue step under roadmap [#10](../../issues/10).

## Communications

`telephony.VoiceProvider` and `messaging.MessagingProvider` are the only interfaces to carriers. Every provider — including the built-in simulator — reports lifecycle progress by delivering **signed webhooks** through the same public gateway. The processor applies them idempotently to the interaction lifecycle (same-state = no-op). Details: [ADR-0005](adr/0005-provider-agnostic-communications.md).

Shipped adapters (5 voice · 3 messaging), all covered by the conformance kit in `internal/comms/conformance`:

| Plane | Adapters |
|---|---|
| voice | `simulator` (built-in, default) · `twilio` (REST + TwiML) · `africastalking` (voice REST) · `freeswitch` (ESL over TCP) · `asterisk` (AMI, Challenge→MD5 login) |
| messaging | `simulator` (built-in, default) · `twilio` (SMS REST + status callbacks) · `whatsappcloud` (Meta Graph v21, 24h window contract) · `africastalking` (SMS + USSD) |

### Provider selection

At startup `cmd/api` loads both planes through `internal/comms/registry` (`registry.LoadFromEnv`): selection is one env var per plane, unknown names / plane mismatches / missing credentials are **fail-closed startup errors that name the offending env vars** (never their values). `internal/comms/factory` then constructs the adapters and wires every one of them — real or simulated — to the same signed-webhook ingest path (`webhooks.Gateway.Ingest` + `webhooks.ComputeSignature`), so no provider ever bypasses signature validation. Unset planes boot the **simulator** and log it explicitly: `provider=simulator reason=not_configured`. Session-based adapters (FreeSWITCH ESL, Asterisk AMI) are closed gracefully on shutdown.

| Plane | `ORVEXA_TELEPHONY_PROVIDER` / `ORVEXA_MESSAGING_PROVIDER` value | Credentials (voice · messaging) | Notes |
|---|---|---|---|
| both | `simulator` *(default when unset)* | none | Built-in deterministic carrier, zero network I/O; honest startup log `provider=simulator reason=not_configured` |
| voice | `twilio` | `ORVEXA_TWILIO_ACCOUNT_SID`, `ORVEXA_TWILIO_AUTH_TOKEN`, `ORVEXA_TWILIO_FROM_NUMBER` | REST + TwiML; TwiML/callback/hold document URLs are deployment endpoints supplied through the factory config — `PlaceCall`/`Hold` fail with typed errors naming the missing knob until provisioned |
| messaging | `twilio` | same three vars as voice | REST; status-callback base URL optional — unset means fire-and-forget sends without receipts (documented posture) |
| messaging | `whatsappcloud` | `ORVEXA_WHATSAPP_PHONE_NUMBER_ID`, `ORVEXA_WHATSAPP_ACCESS_TOKEN`, `ORVEXA_WHATSAPP_APP_SECRET`, `ORVEXA_WHATSAPP_VERIFY_TOKEN` | Meta Graph v21; 24h window contract per adapter README |
| voice | `africastalking` | `ORVEXA_AT_USERNAME`, `ORVEXA_AT_API_KEY`, `ORVEXA_AT_VOICE_PRODUCT_CODE` | Voice REST (`voice.africastalking.com`); status callbacks flow through the signed path |
| messaging | `africastalking` | `ORVEXA_AT_USERNAME`, `ORVEXA_AT_API_KEY`, `ORVEXA_AT_SENDER_ID` | Sender ID is required at boot (fail-closed: per-send failures without one are expensive to diagnose) |
| voice | `freeswitch` | `ORVEXA_FREESWITCH_HOST`, `ORVEXA_FREESWITCH_PORT` *(default 8021)*, `ORVEXA_FREESWITCH_PASSWORD` | Self-hosted ESL; persistent TCP session, closed on teardown |
| voice | `asterisk` | `ORVEXA_ASTERISK_HOST`, `ORVEXA_ASTERISK_PORT` *(default 5038)*, `ORVEXA_ASTERISK_USERNAME`, `ORVEXA_ASTERISK_SECRET` | Self-hosted AMI; Challenge→MD5 login, persistent session closed on teardown |

Least privilege: only the selected provider's variables are read. Credentials are typed `Secret` and rendered redacted on every logging/JSON path (see `internal/comms/registry/doc.go`).

## AI boundary

The AI gateway is the only path to providers (caps: max tokens, timeout, retry classification; every invocation metered as `usage.recorded`). The tool gateway is the only path from AI to side-effects: per-agent allowlists, deny-by-default schemas, rate windows, and audit of every decision including refusals. AI holds no credentials. Decision 5 of [ADR-0001](adr/0001-modular-distributed-architecture.md) is the governing principle; durable workflows (the largest AI-adjacent consumer) are decided in [ADR-0006](adr/0006-durable-workflows.md).

## Identity and authorization

The IdP owns **identity**; Orvexa owns **authorization** ([ADR-0008](adr/0008-oidc-identity-capability-rbac.md)).

- **Two credential shapes, one authenticated tree.** JWT-shaped `Authorization: Bearer` tokens are verified by the OIDC identity plane (`internal/identity`): RS256 only, discovery with issuer binding, JWKS caching with kid rotation (floors against key-endpoint amplification), `exp` required with 30s leeway. Every other credential shape (legacy `orvx_` API keys) flows through the unchanged API-key middleware. A forged JWT is rejected 401 `identity.invalid_token` and is **never retried as an API key**.
- **Capability RBAC.** Roles (`viewer < agent < supervisor < admin`) map to a closed capability catalog (`interaction.read/write`, `case.read/write`, `workflow.run`, `search.read`, `admin.org`). With a database, `role_bindings` rows are authoritative behind a short-TTL resolver — a revocation takes effect within the TTL even while a token is still cryptographically valid. Without a database the IdP-asserted `roles` claim is trusted (static posture, logged). Unknown `(tenant, subject)` resolves to an empty binding → 403.
- **Tenant scope is never client-supplied.** The tenant comes from the verified token's tenant claim (or the hashed API key), never from request bodies; bindings are resolved per `(tenant, subject)` so a cross-tenant claim cannot buy capabilities.

## Search degradation principle

Search is a read-model luxury, never a dependency of the hot path ([ADR-0004](adr/0004-event-delivery-and-outbox.md) consumers, issue #36):

- The OpenSearch indexer consumes the bus through a **bounded queue and sheds events** (drop + structured log) when the backend is slow or down — publishers are never blocked, memory is capped.
- The query path answers a **typed 503** (`search.not_configured` when unset, `search.unavailable` when the backend fails a bounded per-request deadline) — the deployment says "search is off/degraded" instead of pretending.
- PostgreSQL remains the source of truth; reindexing from the database is the recovery path, not per-event retry.
- Every query is filtered server-side by an exact `tenant_id` term derived from the authenticated principal; there is no tenant parameter on the wire and query strings are never logged.

## Infrastructure drivers (env-gated, one per concern)

| Concern | Drivers (selection) | Gate | Degrades to |
|---|---|---|---|
| Event bus | `inproc` (default) · `nats` JetStream | `ORVEXA_BUS_DRIVER`, `ORVEXA_NATS_URL` | in-proc fan-out; worker logs the fallback loudly |
| Agent presence | in-process TTL store (default) · Redis shared cache | `ORVEXA_REDIS_URL` | DB reads — a Redis error never fails a read |
| Rate limiting | in-process token bucket (default) · Redis fixed window (global per tenant) | `ORVEXA_REDIS_URL` | fail-open except auth-critical surfaces (fail-closed 429) |
| Analytics facts | Postgres consumer (default) · batched ClickHouse writer | `ORVEXA_CLICKHOUSE_URL` | Postgres path, byte-identical when unset |
| Conversation search | off (default) · OpenSearch REST | `ORVEXA_OPENSEARCH_URL` | typed 503s; indexer sheds, platform keeps taking calls |
| Workflows | durable Postgres engine (default) · Temporal driver | build tag `temporal` + `ORVEXA_TEMPORAL_URL` | engine path untouched by default ([ADR-0009](adr/0009-temporal-driver.md)) |
| Identity | API-key only (default) · OIDC + capability RBAC | `ORVEXA_OIDC_*` | API-key path byte-identical when unset |

The presence/limiter Redis drivers and the JetStream bus driver ship as tested library swap points (`NewPresenceCacheFromEnv`, `NewRateLimitFromEnv`, `NewNATSFromEnv`); wiring them into the `cmd` binaries is the remaining glue work tracked under roadmap [#10](../../issues/10). The search service, the ClickHouse facts sink and the OIDC identity plane **are** wired into `cmd` today. Every driver is optional: the default profile boots with zero external dependencies.

## Deployment topology

| Deployable | Role | Scale signal |
|---|---|---|
| `cmd/api` | REST v1, dual auth (OIDC / API key), capability-enforced routes, webhook ingress, provider factory | request rate |
| `cmd/worker` | outbox dispatcher; audit consumer; facts consumer (Postgres or ClickHouse sink by env); search indexer; workflow executor | consumer lag, due timers |
| `cmd/realtime` | WebSocket fan-out | connections, msg/s |

All three are stateless besides their storage; run multiple replicas of any. Configuration: `.env.example` (every setting has a safe local default; the platform boots without external dependencies using the in-process bus and the simulator carrier) and the [operations runbook](runbooks/operations.md) for the full variable reference, degradation behaviors and recovery steps.
