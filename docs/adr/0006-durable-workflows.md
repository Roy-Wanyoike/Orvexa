# ADR-0006 — Durable workflows now, Temporal-shaped for extraction

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

Long-running processes (callbacks, collections ladders) must survive restarts, retry deterministically, and expose an auditable step trace. Temporal is the target engine (architecture doc, Domain model — "Workflows are long-running, durable, step-traced processes"), but standing up a Temporal cluster is not required to deliver the product's first workflow-driven behaviors.

## Problem

Ship durable, crash-safe workflows today without foreclosing the Temporal extraction — and without inventing a bespoke engine whose semantics diverge from Temporal's.

## Decision

1. **DB-backed durable executor** with Temporal-compatible semantics:
   - workflow state lives in `workflow_instances` (payload + state + current step);
   - steps are **deterministic functions** of (type, payload, state, step) — replayable;
   - timers are rows (`next_run_at`), advanced by a `FOR UPDATE SKIP LOCKED` claiming loop (safe under concurrent workers);
   - every executed step is recorded in `workflow_steps` (immutable trace);
   - failures mark the instance `failed` with the error preserved — nothing is silently dropped.
2. **Workflows are platform actors**, not AI: they call services directly through a narrow `Services` port (message queuing). They do NOT bypass the tool gateway's audit for AI-originated actions (workflows have no AI origin).
3. **Temporal extraction path:** the engine interface (`Start*`, `Tick`, `Get`, `Cancel`) maps 1:1 to Temporal workflows + activities; extraction = implementing the port with a Temporal client and migrating instances — a deployment change, not a redesign.

## Alternatives considered

- **Temporal from day one:** operational burden (cluster, workers, namespaces) ahead of product-market fit; rejected for the default profile, retained as the extraction path.
- **Cron jobs + task tables:** no deterministic step semantics, no replay, no audit trail; rejected.
- **In-memory schedulers:** lose state on restart — the exact failure durable workflows exist to prevent.

## Consequences

- Workflow timers are second-granular at best (poll interval 15s default); sub-second scheduling waits for Temporal.
- The analytics consumer + facts tables (same wave) keep dashboards off OLTP.

## Security implications

Workflow instances are tenant-scoped and referenced by id; cancel/get validate tenancy. Every step writes an audit-visible event through the outbox.

## Operational implications

`workflow_instances` backlog is observable (due-but-unadvanced rows); failed instances retain their error for operator triage; the executor is safe to run in multiple worker replicas.
