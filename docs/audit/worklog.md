# Orvexa — Engineering Worklog

Running log of autonomous dispatch waves. One entry per task: scope kept
exclusive, evidence linked to the PR.

---

## W-C1b — Platform Engineer (event backbone) — issue #33 [O-24]

**Task:** Complete the NATS JetStream bus driver behind the existing `bus.Bus`
abstraction (continuation: salvaged 5 in-progress files on `feat/nats-bus`).
**Base:** main @ `0a3aedd` · **Branch:** `feat/nats-bus` · **Date:** 2026-09-10

**Delivered (exclusive scope: `internal/platform/bus/{nats.go,nats_test.go,kit_test.go}`):**

- `nats.go` — JetStream driver: stream `orvexa-events` (`orvexa.>`, FileStorage,
  7d retention); synchronous bounded Publish (error when JetStream unavailable,
  no unbounded buffering); durable pull consumer per subscription (fan-out like
  InProc, cursor resumes across restarts); ack on handler success, nak with
  bounded exponential backoff (100ms → 30s cap) on handler error, term on
  undecodable poison messages; `Close` drains subscriptions + connection.
  Opt-in only via `ORVEXA_NATS_URL` (`ErrNATSDisabled` otherwise) so the InProc
  default stays byte-identical. go.mod untouched (nats.go v1.53.1 pre-locked).
- `kit_test.go` — shared Bus conformance kit: publish/subscribe roundtrip with
  field-level fidelity, wildcard, multi-subscriber fan-out, unsubscribe
  (idempotent), invalid envelope/topic rejection, bounded publish, idempotent
  close. InProc run unconditional (`TestInProcBusConformance`).
- `nats_test.go` — in-process JetStream-shaped fake (no binaries downloaded):
  conformance parity run, wire-format/subject tables, consumer-config tables,
  backoff ladder table (capped, overflow-free), ack/nak/term semantics,
  lifecycle + gating tests, pure-helper tables (`matchFilter`, `durableFor`).
- `nats_integration_test.go` — build tag `integration`, runtime skip unless
  `ORVEXA_NATS_URL` set; live-server kit + redelivery + durable-restart proofs.
- `bus.go` — package doc records the NATS profile (comment-only; zero drift).

**Verification:** `gofmt -l .` empty · `go vet ./...` clean (incl.
`-tags=integration`) · `go build ./...` clean · `go test -race ./...` all
packages green · `make lint-todos` clean. InProc path byte-identical (no
pre-existing test file touched; reference kit green).

**PR:** "feat(bus): NATS JetStream driver behind the Bus abstraction (#33)" —
Closes #33.

**Risks / follow-ups:** live-server evidence pending a compose-capable
environment (integration suite skips cleanly today); consumer sharding and
KV/ObjectStore explicitly out of scope per issue; `ORVEXA_BUS_DRIVER=nats`
profile wiring into binaries lands with the deployment wave.
