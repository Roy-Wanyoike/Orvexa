# ADR-0005 — Provider-agnostic communications via hexagonal ports

- **Status:** Accepted
- **Date:** 2026-09-10

## Context

Orvexa's core must work with any voice/messaging carrier. Carrier APIs differ wildly (webhook shapes, auth, leg semantics), and carriers change (price, reliability, coverage). A contact-center platform hard-wired to one carrier is hostage to that carrier.

## Problem

How do telephony and messaging integrate without letting provider concerns leak into the domain core, and how does the platform develop and test the full interaction loop before carrier integrations exist?

## Decision

1. **Ports own the language.** `telephony.VoiceProvider` (PlaceCall/Hangup/Transfer/Hold/Resume) and `messaging.MessagingProvider` (Send) are the ONLY interfaces between the domain core and carriers. The core speaks interaction lifecycle, never carrier dialects.
2. **Adapters are leaves.** Every carrier — including the built-in simulator — lives behind the ports. Adapters translate native payloads into the platform `ProviderEvent` shape and are the ONLY place provider-specific fields exist.
3. **All lifecycle truth flows through signed webhooks.** Providers (real or simulated) report ringing/connected/ended/delivered/read by POSTing HMAC-signed payloads to the public webhook gateway. No provider gets API keys; no side path bypasses signature validation. The simulator uses the identical entry point as production carriers — so the loop it exercises IS the production loop.
4. **The provider drives lifecycle transitions; the platform never duplicates them.** The processor maps provider events to guarded interaction transitions idempotently (same-state delivery = no-op). Operator actions (hangup/transfer/hold) command the provider; the resulting state change arrives via webhook. This keeps a single writer per transition source.
5. **Simulator is a first-class adapter**, not test scaffolding: it is deterministic (configurable pace), performs no network I/O, is labelled `provider=simulator`, and enables the full E2E loop in CI with zero external dependencies.

## Alternatives considered

- **Thick carrier SDKs in the core:** couples core to vendor SDK release cycles; rejected.
- **Provider polling instead of webhooks:** latency on the interaction critical path; rejected (webhook-first with polling as adapter detail if a carrier requires it).
- **Test-only mock providers:** mocks would bypass the signed-webhook entry and test a loop production never runs; the simulator avoids that lie.

## Consequences

- New carrier = new adapter + config; no core changes.
- The webhook gateway is load-bearing for the interaction plane — its availability matters like the core's.
- Lifecycle debugging means reading the provider_events ledger (every provider delivery is persisted with its signature validity).

## Security implications

Signature validation is timing-safe HMAC-SHA256; unsigned ingestion is refused. Tenant context flows via provider options configured server-side, never from webhook payloads alone (payload tenant hints are cross-checked in real-carrier adapters).

## Operational implications

Simulator pacing is config-driven (`Pace`); production deployments configure real carriers per the operations runbook. Unknown-leg and unknown-event errors are surfaced (4xx to carriers), enabling provider-side monitoring.
