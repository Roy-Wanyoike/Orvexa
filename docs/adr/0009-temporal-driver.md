# ADR-0009: Temporal-backed workflow driver (build-tagged adoption)

**Status:** Accepted
**Date:** 2026-09-11
**Deciders:** Autonomous engineering organization (owner-directed roadmap item #10 / issue #34)

## Context

`internal/workflows` ships a durable workflow engine (callback + collections) on
Postgres persistence with its own timer/retry semantics. The production roadmap
(issue #10) lists Temporal as an infrastructure option. Enterprise deployments
frequently operate a shared Temporal fleet and prefer workflow execution on it
rather than a second engine.

The engine's public surface is concrete (`*workflows.Engine` taken directly by
the HTTP layer), and its persistence/queue interfaces are internal. The
 Temporal SDK (`go.temporal.io/sdk`) is a heavy dependency whose server is not
available in this environment's verification loop.

## Problem

How do we honor the roadmap's Temporal line item without (a) destabilizing the
proven engine path, (b) requiring a Temporal server for tests, or (c) shipping
unverifiable claims?

## Decision

- Ship `internal/workflows/temporaldriver` **exclusively under build tag
  `temporal`**. Default builds are byte-identical to today; the SDK is imported
  only when the tag is set.
- The driver implements the engine's workflow lifecycle
  (StartCallback / StartCollections / Get / Cancel) with Temporal owning
  execution (durable timers, retries) and the existing Store port owning the
  audit trail the read API serves from.
- It is deliberately **not** a drop-in `*workflows.Engine` replacement: adoption
  is a per-deployment wiring decision (documented below), not a code swap.
- Unit tests run against a mocked Temporal `Client` interface (fakes in
  `fake_test.go`); an integration test gates on `ORVEXA_TEMPORAL_URL` and skips
  cleanly when unset.
- A `temporal` compose profile (temporalio/auto-setup + UI) documents the local
  server path for docker-capable environments.

## Alternatives considered

1. **Replace the engine with Temporal wholesale** — rejected: breaks the
   verification loop (no server here), risks a regression in the release gate,
   and couples all deployments to an extra control plane.
2. **Ignore the roadmap line** — rejected: dishonest to the owner's stated
   infrastructure requirements.
3. **Runtime-selected driver without build tag** — rejected: the SDK's
   transitive weight would burden every binary for an opt-in feature.

## Consequences

- Zero risk to the default path (tag isolates 100% of the code).
- Adoption requires a small wiring change in `cmd/api` where the deployment
  chooses the driver — an explicit, reviewable operation.
- Temporal-specific semantics (durable timers, retry policies) are inherited
  from the server; engine-native semantics remain for the default path. The two
  paths are documented as distinct, not interchangeable.

## Security implications

No new credentials beyond `ORVEXA_TEMPORAL_URL` (+ optional mTLS material via
the SDK's own client options). The driver never logs workflow payloads beyond
existing field redaction rules; Temporal namespace credentials come from the
environment, never code.

## Operational implications

Operators adopting the driver must run (or reach) a Temporal server, monitor it
alongside Orvexa, and apply the compose profile or a production equivalent.
Engine-native deployments are unaffected.

## Migration plan

1. Deploy with the `temporal` build tag in a staging namespace.
2. Point `ORVEXA_TEMPORAL_URL` at the fleet; wire the driver instead of the
   concrete engine in `cmd/api`.
3. Validate workflow journeys (callback/collections) with the existing E2E
   suite; cut over; keep the default build available for instant rollback.
