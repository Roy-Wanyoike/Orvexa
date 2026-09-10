# Orvexa Architecture

The authoritative decisions live in the [ADRs](adr/). This document is the map.

## The four planes

```text
                 ┌───────────────────────────┐
                 │   Clients · Providers     │
                 └─────────────┬─────────────┘
                               │
              API (auth, tenancy, rate limits, webhooks)
                               │
        ┌──────────────────────┼──────────────────────┐
        ▼                      ▼                      ▼
  INTERACTION PLANE     EXECUTION PLANE        INTELLIGENCE PLANE
  customers             workflows              ai gateway + runtime
  conversations         (callback,             tool gateway (AI→actions)
  interactions           collections)          analytics facts
  telephony/messaging                                  │
  routing + presence                                   │
        │                      └──────────┬───────────┘
        ▼                                 ▼
        └──────────► EVENT BUS (outbox → at-least-once) ◄──────────┘
                               │
                      CONTROL PLANE (everywhere)
              tenancy · api keys · scopes · audit · usage metering
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

## Communications

`telephony.VoiceProvider` and `messaging.MessagingProvider` are the only interfaces to carriers. Every provider — including the built-in simulator — reports lifecycle progress by delivering **HMAC-signed webhooks** through the same public gateway. The processor applies them idempotently to the interaction lifecycle (same-state = no-op). Details: [ADR-0005](adr/0005-provider-agnostic-communications.md).

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

The AI gateway is the only path to providers (caps: max tokens, timeout, retry classification; every invocation metered as `usage.recorded`). The tool gateway is the only path from AI to side-effects: per-agent allowlists, deny-by-default schemas, rate windows, and audit of every decision including refusals. AI holds no credentials. Details: [ADR-0006](adr/0006-durable-workflows.md) companion principles in [ADR-0001](adr/0001-modular-distributed-architecture.md).

## Deployment topology

| Deployable | Role | Scale signal |
|---|---|---|
| `cmd/api` | REST v1, auth, webhook ingress | request rate |
| `cmd/worker` | outbox dispatcher, audit + analytics consumers, workflow executor | consumer lag, due timers |
| `cmd/realtime` | WebSocket fan-out | connections, msg/s |

All three are stateless besides their storage; run multiple replicas of any. Configuration: `.env.example` (every setting has a safe local default; the platform boots without external dependencies using the in-process bus and the simulator carrier).
