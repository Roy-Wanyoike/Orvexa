# ADR-0007 — Multi-region readiness: region isolation design (design-only)

- **Status:** Accepted
- **Date:** 2026-09-12

## Context

Contact-center buyers ask a predictable question: what happens when a region fails, and where does our data live. Today Orvexa is a single-region deployment: one PostgreSQL cluster as the system of record, one outbox/event bus backbone, three stateless binaries, and providers configured per environment. Nothing regional exists in the code, the tests, or the verification evidence.

What does exist is an architecture whose seams are clean: every deployable is stateless besides its storage (architecture doc, Deployment topology), all cross-domain coordination flows through the event backbone ([ADR-0004](0004-event-delivery-and-outbox.md)), and the interaction critical path (telephony → routing → agent) never depends on anything outside the process's own storage.

## Problem

How do we make multi-region a credible, low-regret path — without shipping active/passive or active/active claims we cannot evidence, and without retrofitting the product before any deployment needs it?

## Decision

**Region isolation is adopted as the design unit; nothing multi-regional is claimed as implemented.** The design constraints below are the readiness commitment:

1. **A region = (regional edge + full Orvexa stack + regional PostgreSQL).** All three binaries are stateless besides storage, so a region is a deployment of exactly what ships today — not a code fork, not a special build.
2. **Tenant→region assignment is a provisioning-time, control-plane concern.** Tenant identifiers are UUIDs with no region encoding; the mapping lives in provisioning configuration, never in data. A tenant's interactions, audit trail, and provider credentials are region-local.
3. **The interaction critical path stays region-local.** Ingest → persist → route → agent must never cross a region boundary; cross-region traffic is confined to control-plane aggregation (audit views, analytics roll-ups) and is always asynchronous.
4. **Event backbone is per-region.** The transactional outbox and the bus are regional infrastructure; cross-region event fan-out is a future decision, not an assumed property. Consumers dedupe by envelope id, so a future bridge cannot create double-effects.
5. **Audit is append-only per region.** A global audit view is an aggregation concern; regional append-only stores are never merged destructively.
6. **Communications and drivers are region-agnostic configuration.** Provider selection, credentials, and driver gates (`ORVEXA_*`) are per-region environment; the registry/factory model ([ADR-0005](0005-provider-agnostic-communications.md)) needs no region concept.

## Explicitly NOT done (read this before quoting this ADR)

- **No cross-region replication is implemented or claimed** — not synchronous, not asynchronous, for any table.
- **No active/passive or active/active deployment has been built or tested.** No failover runbook exists.
- **No RPO/RTO numbers are claimed** — any number in a proposal today would be invented.
- **No global tenant→region routing layer (latency-based DNS, geo-steering, global load balancing) ships.**
- **No multi-region test exists in the verification matrix.** The authoritative gate ([ADR-0003](0003-verification-without-hosted-actions.md)) is single-region; region isolation is currently design discipline, not verified behavior.

## Alternatives considered

- **Do nothing until a buyer requires it:** cheapest now, but region-implicit assumptions (region encoded in identifiers, cross-region joins in domain SQL) are expensive to retrofit; the design constraints above cost nothing because they match what already ships. Rejected as the sole posture; adopted as the trigger for this ADR.
- **Ship an active/passive failover now:** would be an unverifiable claim (no second region exists in the verification loop) and violates the evidence-first discipline. Rejected.
- **Region-sharded single cluster (one Postgres, per-tenant schemas):** conflates tenant isolation with region isolation and caps the blast radius of neither. Rejected.

## Consequences

- A tenant is pinned to its region; cross-region tenant migration is an application-level concern for a future ADR, not a hidden side-effect of replication.
- UUIDs (not serials) everywhere keep identifiers globally collision-free without cross-region coordination — a constraint already satisfied by the schema.
- Every new component must answer "is this region-local or cross-region" in its ADR or review; the cost of the design is this one question, asked continuously.
- The enablement path is incremental: pick the replication strategy when a real deployment requires it; nothing above forecloses sync vs async.

## Security implications

- Credentials are regional scope: `ORVEXA_WEBHOOK_HMAC_SECRET`, provider credentials, and database credentials are per-region values; a region compromise does not mint webhooks for another region.
- The audit trail stays append-only per region; cross-region aggregation must never become a write path.
- Tenant→region mapping is control-plane data and must be protected as access-routing policy (it determines which region may serve a tenant).

## Operational implications

- A region boots exactly like today's single-region deployment — the [operations runbook](../runbooks/operations.md) applies per region unchanged, which is the readiness payoff of the design.
- Region health is currently single-region health (`/healthz`, `/readyz` truthfulness); regional dashboards and failover drills are explicitly future work with their own acceptance criteria and evidence.
