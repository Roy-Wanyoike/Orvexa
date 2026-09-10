# ADR-0001 — Modular distributed architecture on Go

- **Status:** Accepted
- **Date:** 2026-09-10
- **Deciders:** Orvexa founding engineering

## Context

Orvexa is an AI-native Contact Center OS: omnichannel interactions (voice, WhatsApp, SMS, email, chat, USSD), human + AI agents, routing, workflows, integrations and analytics. Telephony-scale systems fail when every subsystem sits on the interaction critical path, and they rot when domain boundaries blur.

## Problem

Choose the service topology and the architectural rule-set that lets a small team ship a complete contact-center platform without a 30-service microservice sprawl, while preserving a path to extract high-scale components later.

## Decision

1. **Modular distributed architecture, not a microservice free-for-all.** Strong domain boundaries inside approximately eight deployables:

   ```text
   api · interaction-core · communications · routing · ai-runtime
   workflow-workers · analytics-workers · realtime-gateway
   ```

   In this repository the deployables are the `cmd/*` binaries; domain modules under `internal/*` own their tables and may not reach into another module's storage.

2. **Four-plane model** as the foundational boundary set:
   - **Interaction Plane** (realtime, critical path): channels, conversations, interactions, routing, agents, queues.
   - **Intelligence Plane**: AI gateway, agents, transcription, RAG, QA, analytics.
   - **Execution Plane**: workflows, tool gateway, integrations, payments, notifications.
   - **Control Plane**: organizations, tenants, identity, authorization, billing/metering, audit, configuration.

3. **Event-driven backbone with transactional outbox.** Domain state changes commit together with their events; a dispatcher publishes to the bus (NATS JetStream-shaped). Consumers fail independently. Nothing outside the Interaction Plane may block a call.

4. **Provider-agnostic communications.** Core sees `VoiceProvider`/`MessagingProvider` interfaces; Twilio, Africa's Talking, WhatsApp Cloud, FreeSWITCH etc. are adapters. The platform owns the abstraction.

5. **AI never bypasses business rules.** All AI side-effects flow through the Tool Gateway (authorization, tenant validation, schema validation, rate limit, audit). AI holds no database/cloud/payment credentials.

6. **One PostgreSQL cluster with logical domain ownership** initially; large domains (interactions, audit, webhook deliveries) designed for time-partitioning later. Analytics aggregations live in a facts store (ClickHouse-shaped), never in OLTP queries.

## Alternatives considered

- **Conventional monolith (single binary, shared DB, no events):** fastest start, but channel/AI/workflow scales and failure modes differ; retrospective extraction is expensive.
- **Full microservices from day one (30 services):** operational burden without corresponding scale; distributed transactions and versioning overhead would dominate.
- **Serverless-first:** poor fit for long-lived interactions, WebSocket fanout and deterministic workflow semantics.

## Consequences

- Wave-based delivery: each wave lands a coherent vertical slice behind an interface.
- Local-safe default profile (in-process bus, simulator providers, durable-DB workflows) keeps CI infra-free; production adapters are config-level swaps tracked as issues.
- Requires discipline enforcing module ownership — enforced via code review + the ownership map in `docs/domains.md`.

## Security implications

Control-plane primitives (hashed API keys, tenant resolution middleware, append-only audit) land in the foundation wave so every subsequent feature inherits them by construction.

## Operational implications

Every deployable exposes `/healthz` (liveness) and readiness truth; graceful shutdown everywhere; metrics counters for bus lag and consumer failures from the wave that introduces them.
