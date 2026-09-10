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

---

## Wave C5b — [O-28] issue #37: Redis drivers (agent presence cache + distributed rate-limit store)

**Owner:** Agent C5 (Platform engineer, perf/state) · **Branch:** `feat/redis-drivers` · **Predecessor state:** 2 untracked files salvaged (`presence_redis.go`/.`_test.go`), re-verified, restructured, extended; everything else written fresh in this session.

**Delivered:**
- `internal/routing/presence_redis.go` — `RedisPresenceCache` implementing the same
  `PresenceReader` + `Invalidate` surface as the in-process `PresenceStore` (additive file;
  `presence.go` untouched). DB (`agent_presence`) stays the source of truth; Redis is a shared
  TTL snapshot cache (`orvexa:presence:v1:<tenant>`, sorted-JSON agent-ID array, negative
  caching of known-empty sets); `Invalidate` DELs the shared key so every node sees the drop.
  Redis errors NEVER fail a read — dial/command/decode failures degrade to the DB read (logged,
  no keys/URLs/credentials echoed); only DB errors propagate, exactly like the in-process store.
  Env swap point `NewPresenceCacheFromEnv`: `ORVEXA_REDIS_URL` unset → the exact in-process
  `*PresenceStore` (byte-identical default); set-but-malformed → error (value never echoed);
  set-but-unreachable → driver still constructs and degrades until Redis recovers.
- `internal/platform/httpx/limiter_redis.go` — `RedisRateLimit` implementing the exact
  `Allow(key string, now time.Time) (bool, time.Duration)` signature of the token bucket via the
  new `Limiter` interface (both drivers asserted against it). Server-side INCR + EXPIRE NX fixed
  window keyed `orvexa:rl:v1:<scope>:<key>:<bucket>` — tenant principal (or client IP) + route
  class (scope), so limits are GLOBAL across a multi-binary deployment; `now`-derived buckets
  need NTP-synced node clocks (documented). Fail semantics implemented and documented exactly:
  `FailClosed=false` → allow on any Redis error (open elsewhere); `FailClosed=true` → deny with
  the remaining window (auth-critical surfaces, 429 + Retry-After); no silent in-process
  fallback (hybrid policy would be node-dependent and untestable — documented). Honest
  divergences from the token bucket stated in the doc comment (fixed window vs refill, no burst
  split, maxBuckets meaningless, denied attempts still INCR, ≤2× limit across a boundary).
  Env swap point `NewRateLimitFromEnv` mirrors the presence one.
- Tests (both packages): in-package fakes of the go-redis `UniversalClient` surface (embedded
  nil interface — only the commands the drivers issue are implemented; no new deps), plus RESP2
  TCP harnesses proving the drivers against the REAL go-redis client (dial, serialization,
  `redis.Nil` miss contract, TTL stamping, INCR/EXPIRE NX). Swap equivalence suite proves the
  Redis limiter and `NewRateLimit` make identical decisions for the burst pattern (and asserts
  the documented cross-boundary divergence). Integration suites (`-tags=integration`, separate
  files, house style): `ORVEXA_REDIS_URL`-gated against the compose `redis:7` service, skipping
  cleanly when unset — presence lifecycle (miss→load→write-back→hit→invalidate→TTL expiry) and
  the global-budget property (two independent clients share one bucket; capacity restored for
  both in the next window; TTL bounded by the window).
- Docs: `.env.example` + operations runbook — `ORVEXA_REDIS_URL` row with per-driver outage
  semantics, compose dev path, integration-suite invocation, scaling notes (multi-replica API
  once Redis is shared; NTP clock requirement).

**Salvage notes:** predecessor's two untracked files were compile-clean and behaviorally sound;
kept (after restructuring: compose integration test moved to the `-tags=integration` file per
house style; unused imports trimmed; gofmt). Limiter driver + all four limiter/presence test
harnesses and docs were written in this session.

**Verification (CI-equivalent local full matrix):** `gofmt -l .` empty · `go vet ./...` clean
(incl. `-tags=integration`) · `go build ./...` clean · `go test -race ./...` all 29 packages
green · `make lint-todos` clean · integration suites green against a REAL Redis 7.2.5
(source-built in-sandbox, compose unavailable here): `TestRedisPresenceComposeIntegration` PASS
(1.10s), `TestRedisRateLimitComposeIntegration` PASS (2.16s); both skip cleanly without the env.

**PR:** "feat(infra): Redis presence cache + distributed rate-limit drivers (#37)" — Closes #37.

**Risks / follow-ups:** glue wave must call `NewPresenceCacheFromEnv` / `NewRateLimitFromEnv`
in `cmd/api` + `httpserver` wiring (the swap points exist and are proven; until then the env var
affects nothing at boot — by design, zero default drift); limiter `scope` values must be chosen
at the mount sites (`auth` vs `webhooks`) with `FailClosed=true` on the auth surface; fixed
window permits up to 2× limit across a bucket boundary (documented; a ZSET sliding window is the
follow-up if that is unacceptable); no Redis Cluster/pub-sub scope by issue constraint.
