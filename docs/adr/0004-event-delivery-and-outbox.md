# ADR-0004 — Event delivery: transactional outbox + bus abstraction

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

Orvexa is event-driven: domains publish facts, other planes consume them independently. The failure mode to eliminate is "database updated but event lost" (and its twin, "event delivered twice = two side-effects").

## Problem

How do domain writes and event publication stay atomic, and what delivery semantics do consumers design against — before and after the NATS JetStream production driver lands?

## Decision

1. **Transactional outbox is the only sanctioned event path.** Domain services insert envelopes into `outbox_events` inside the SAME SQL transaction as their state change (`outbox.Writer.Insert(ctx, tx, env)`). No domain code publishes directly to the bus.
2. **Dispatcher with lease semantics.** The worker claims batches `FOR UPDATE SKIP LOCKED` (no double-publish across workers), publishes to the bus, and acks. Crashes between publish and ack re-deliver — delivery is **at-least-once**; consumers dedupe by `Envelope.ID` (unique per event).
3. **Failed batches retry with a cap, then park** as `status='failed'` for operator inspection — events are never silently deleted.
4. **Bus abstraction** (`platform/bus`): `InProc` driver (default; fan-out with bounded queues, per-subscriber goroutines) + NATS JetStream driver as an infrastructure-wave adapter behind the same interface. Config selects the driver; consumers are driver-agnostic.
5. **Topic registry is closed.** `pkg/events` validates every envelope against the registry — ad-hoc topics cannot appear in the wild.
6. **Inbound provider webhooks** persist to the `provider_events` ledger with a derived idempotency key before any domain processing; replays return the original event id (single-effect ingestion).

## Alternatives considered

- **Direct publish after commit:** loses events on crash between commit and publish — the exact failure the outbox eliminates.
- **Change-data-capture (Debezium):** heavier operational surface than the codebase needs pre-scale; revisit at extraction (O-10).
- **Exactly-once delivery at the bus layer:** impossible in general; at-least-once + idempotent consumers is the honest contract.

## Consequences

- Consumers must be idempotent by contract (documented in `pkg/events`).
- Dispatcher lag is a first-class operational metric (worker heartbeat logs backlog).
- NATS extraction becomes a driver implementation, not a redesign.

## Security implications

Webhook ingestion validates HMAC signatures (timing-safe) and caps payload size; unsigned ingestion is refused outright. The outbox carries tenant ids so downstream consumers remain tenant-scoped.

## Operational implications

`outbox_events` grows with published rows; the analytics wave adds retention (tracked in O-8 scope). Dispatcher metrics expose pending/publishing/failed counts.
