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

## AI boundary

The AI gateway is the only path to providers (caps: max tokens, timeout, retry classification; every invocation metered as `usage.recorded`). The tool gateway is the only path from AI to side-effects: per-agent allowlists, deny-by-default schemas, rate windows, and audit of every decision including refusals. AI holds no credentials. Details: [ADR-0006](adr/0006-durable-workflows.md) companion principles in [ADR-0001](adr/0001-modular-distributed-architecture.md).

## Deployment topology

| Deployable | Role | Scale signal |
|---|---|---|
| `cmd/api` | REST v1, auth, webhook ingress | request rate |
| `cmd/worker` | outbox dispatcher, audit + analytics consumers, workflow executor | consumer lag, due timers |
| `cmd/realtime` | WebSocket fan-out | connections, msg/s |

All three are stateless besides their storage; run multiple replicas of any. Configuration: `.env.example` (every setting has a safe local default; the platform boots without external dependencies using the in-process bus and the simulator carrier).
