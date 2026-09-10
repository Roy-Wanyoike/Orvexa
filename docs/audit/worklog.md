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

---

## W-C4b — Senior Backend Engineer (search) — issue #36 [O-27]

**Task:** OpenSearch conversation search — event-driven indexer + tenant-scoped
search API (continuation: salvaged 6 in-progress files on
`feat/opensearch-search`, none committed; two tests failed under `-race`).
**Base:** main @ `0a3aedd` (merged `origin/main` @ `be4eee6` before PR) ·
**Branch:** `feat/opensearch-search` · **Date:** 2026-09-10

**Delivered (exclusive scope: `internal/search/`,
`internal/httpserver/search_handlers.go(+_test)`, `api/openapi/orvexa-v1.yaml`
additive, `docker-compose.dev.yml` additive, env/runbook docs):**

- `internal/search/indexer.go` — bus consumer over the six
  conversation/interaction topics; bounded queue (default 1024) with a strict
  drop + structured-log shedding policy (Handle never blocks, never returns a
  retry signal — publishers stay unblocked, memory is capped); envelope-id
  dedupe ring (at-least-once contract) and interaction→conversation mapping
  ring, both FIFO-eviction bounded; projects into `orvexa-conversations` via
  plain `net/http` REST (no client dep) with `tenant_id` mandatory on every
  document; idempotent index creation with tenant keyword mapping.
- `internal/search/service.go` — tenant-scoped `_search`: the tenant term
  filter is baked server-side from the authenticated caller (no tenant
  parameter exists on the wire), `simple_query_string` over the projection,
  size caps (default 25 / max 50 / max query length 256), per-request
  deadline; typed errors: `search.not_configured` and `search.unavailable`
  (kind `unavailable` declared in-package; pkg/errors untouched), both 503-class.
- `internal/httpserver/search_handlers.go` — self-contained
  `MountSearchRoutes(r, svc, principalFn)` → `GET /api/v1/search/conversations?q=&limit=`
  (server.go untouched; orchestrator mounts inside the authed v1 group);
  tenant ONLY from the principal (principal-less requests fail closed 401,
  never reach the backend); degradation comments document why a search outage
  never blocks the hot path; unwired/nil service answers the honest typed 503.
- Tests — `fake_test.go` in-memory OpenSearch (httptest) that REFUSES
  filterless queries (400 tripwire: a service regression cannot leak
  cross-tenant rows); indexer tests (lifecycle projection in causal order,
  Handle-never-blocks drop proof, outage accounting + recovery, replay dedupe,
  not-configured no-op, unknown-mapping skip, bounded ring eviction); service
  tests (tenant isolation both directions + cross-tenant-nothing proof +
  every-request-filter proof, 503 on fast-503 and dead backends, validation
  and size caps, hit shape); handler transport tests (envelope shape, tenant
  derivation, limit cap/default, 422, 503s, nil-service, fail-closed);
  `integration_opensearch_test.go` (`-tags=integration`, skips unless
  `ORVEXA_TEST_OPENSEARCH_URL` or `ORVEXA_OPENSEARCH_URL` set) — real-engine
  indexing, tenant isolation and degradation.
- Salvage fixes: `indexer_test.go` was space-indented (gofmt-dirty) and
  asserted `Failed >= 20` with a queue of 4 — impossible under the drop
  policy (now: ≥1 failed write + full disposition accounting); the lifecycle
  test published 4 events unordered on one document and raced the bus's
  unordered fan-out (now publishes in causal order with per-step waits);
  integration gate extended to `ORVEXA_OPENSEARCH_URL`.
- Contract & infra: OpenAPI additive `GET /search/conversations` (schema,
  422, degradation 503s documented); `docker-compose.dev.yml` additive
  `opensearch:2` (single node, loopback, security plugin dev-disabled) +
  `orvexa-dev-osdata` volume; `.env.example` + operations runbook env table
  (`ORVEXA_OPENSEARCH_URL`, optional `ORVEXA_OPENSEARCH_INDEX`).
- Merge hygiene: relocated the `temporal`/`temporal-ui` services under
  `services:` — `origin/main` had appended them after the `volumes:` key
  (invalid compose placement); content otherwise preserved.

**Verification:** `gofmt -l .` empty · `go vet ./...` clean (incl.
`-tags=integration`) · `go build ./...` clean · `go test -race ./...` all
packages green (search package stress-run `-count=3`) · `make lint-todos`
clean · integration test skips cleanly without the env var.

**PR:** "feat(search): OpenSearch conversation indexer + tenant-scoped search
API (#36)" — Closes #36.

**Risks / follow-ups:** glue PR must call `MountSearchRoutes(authed,
deps.Search, principal)` and start the indexer against the outbox dispatcher's
bus (route 404s today by design until mounted); dropped events recover only
via later events or a reindex-from-outbox job (reindex tool tracked
separately); integration evidence on a real cluster pending a
compose-capable runner.
