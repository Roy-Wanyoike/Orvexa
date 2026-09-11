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

## W-C3c — Data Engineer (ClickHouse facts) — issue #35 [O-26]

**Branch:** `feat/clickhouse-facts` — continued from 4 pushed commits (store,
env gating, fake-driver tests, knob tests) + 3 dirty items; ended at PR
"feat(analytics): ClickHouse facts writer — batched, non-blocking, env-gated
(#35)".

**Completion path (C3c):**
- Reviewed the pushed store end-to-end (buffering/backpressure model, flusher
  ownership, flush-on-close, requeue/shed policy); `go test -race
  ./internal/analytics/...` green before any change.
- `migrations/0011_clickhouse_facts.sql` completed: header documents the
  engine-specific contract (NOT applied by the PG runners — applied via
  compose init on first volume init, or manually with
  `clickhouse-client --multiquery`); event type = target table mirroring the
  Postgres consumer's topic routing; sort key
  `(tenant_id, toStartOfHour(occurred_at), event_id)` — per-tenant hourly
  dashboard layout with `event_id` as the uniqueness tiebreaker so
  ReplacingMergeTree FINAL collapses redeliveries only (a bare
  `(tenant_id, toStartOfHour(ts))` key would collapse ALL distinct events per
  tenant-hour — data loss); monthly partitions + 13-month TTL.
- Salvage verdict on the dirty Makefile/devstack.sh diffs: functionally
  correct and ClickHouse-related (both PG runners skip `*clickhouse*` files),
  but the devstack.sh heredoc had tabs converted to spaces repo-wide (~230
  lines of whitespace churn, embedded pgtool no longer gofmt-clean). Reset and
  re-applied whitespace-faithfully; added the missed `PGTOOL_SRC_VERSION`
  bump 3→4 (without it a stale pgtool would feed ClickHouse DDL to pgx and
  fail the migration loop).
- `docker-compose.dev.yml` additive `clickhouse:24` (native 19000 / HTTP 18123
  host ports mirroring the 55432 convention, `clickhouse-client` healthcheck,
  nofile ulimits) + init-script mount of 0011 + `orvexa-dev-clickhouse`
  volume; merged latest `origin/main` first (#74 relocated temporal services
  and added `opensearch`) — no conflicts.
- Added the missing integration-tagged evidence test
  (`internal/analytics/clickhouse/integration_test.go`, `//go:build
  integration`, env-gated on `ORVEXA_TEST_CLICKHOUSE_URL` / fallback
  `ORVEXA_CLICKHOUSE_URL`, skip-clean): re-applies the idempotent 0011 DDL,
  drives facts + a byte-identical redelivery through the real batching path,
  asserts FINAL dedup, Summary group-by shape and sum(amount) against a live
  engine.
- `go mod tidy`: clickhouse-go/v2 → direct (stale `// indirect` marker), same
  fix for nats.go/temporal/websocket (direct since #72/#73); stale unused
  indirects pruned; go.sum graph completed.
- Docs: additive `docs/devstack.md` ClickHouse section (gate contract, compose
  path, DDL application paths, retention notes, knobs, evidence command).

**Verification:** `gofmt -l .` empty · `go vet ./...` clean · `go build ./...`
clean (also `-tags=integration`, `-tags=temporal`) · `go test -race ./...`
exit 0 (30 packages ok, 0 FAIL) · `make lint-todos` clean · `make migrations`
stub-psql dry run: 0001–0010 apply, 0011 skipped · devstack.sh `bash -n` +
extracted embedded pgtool gofmt/vet/build clean · compose YAML parsed
(services/volumes verified) · integration test skips cleanly without env
(live-engine evidence pending a compose-capable runner — no docker in the
authoring sandbox).

**Risks / follow-ups:** worker-side wiring of `clickhouse.FromEnv` behind
`ORVEXA_CLICKHOUSE_URL` is outside this task's exclusive file scope (selection
point is exported and documented); `go vet -tags="temporal integration"` flags
a pre-existing `internal/workflows/temporaldriver/integration_test.go:88`
compile error inherited from #34 (unrelated, untouched); 13-month TTL is a
placeholder until formal retention policy lands.

---

## Wave D1 — [O-30] issue #39: Independent security sweep — PUBLIC-VISIBILITY GATE (agent D1)

**Branch:** `security/sweep` · **Base:** main @ `6459bc5` · **Ownership honored:** only
`scripts/security-scan.sh`, `docs/security/`, `internal/platform/httpx/` (header+limiter,
gaps proven first), `.env.example` (security vars). Cross-surface defects filed, not patched.

**Deliverables:**
- `scripts/security-scan.sh` — self-contained secret scanner (bash+git+python3 stdlib,
  zero installs/network): 12 checks (AWS/Slack/OpenAI/Google/GitHub tokens, private-key
  blocks, credential URLs, ≥40-char high-entropy strings at Shannon ≥ 4.5 bits/char,
  tracked `.env` files, secret-named artifacts, generic credential assigns), explicit
  per-hit allowlist justification, redaction-only output, exit 0/1/2. Canary-validated:
  11/11 planted secret shapes detected (exit 1); tracked tree CLEAN (exit 0), 261 files.
- govulncheck ./... triaged (9 findings): **1 symbol-level reachable — GO-2026-5004
  (pgx < v5.9.2) via cases.Service.ListNotes → Pool.Query → sanitize.SanitizeSQL**;
  exploitability assessed (all Orvexa SQL is extended-protocol parameterized; no
  SanitizeSQL/simple-protocol usage) but a SQL-injection-class finding with a same-major
  patch does not ride a public repo → bump filed as **#81**, verdict BLOCKED on it.
  chi RealIP spoofing vulns (GO-2026-5777/5775) verified unreachable (RealIP unused;
  clientIP = RemoteAddr).
- `internal/platform/httpx/middleware.go` — CSP (`default-src 'none'; frame-ancestors
  'none'; base-uri 'none'`) + HSTS (`max-age=31536000; includeSubDomains`) added to the
  global SecurityHeaders middleware (JSON-only API ⇒ maximal CSP is free; HSTS sent
  unconditionally per RFC 6797 + edge-TLS topology). Tests: exact header matrix on 2xx,
  4xx and panic-recovery paths.
- `internal/platform/httpx/middleware.go` — limiter eviction O(n²)→O(n log n): measured
  ~150 µs/op sustained under unique-key flood at the 10k-bucket cap (algorithmic
  DoS), now ~1.0 µs/op; policy unchanged (oldest half by last touch); committed
  benchmark + eviction-policy test pin it.
- Rate-limit matrix: 24/24 mutation routes covered (23 POST + 1 PUT), keys are
  server-side only (`principal.APIKeyID` / `"oidc:"+verified sub` / socket RemoteAddr);
  no client-supplied key input exists.
- Webhook ingress reviewed read-only: fail-closed paths, verifier registry (#24)
  normalization/precedence, dedupe single-effect — evidenced by the existing adversarial
  suite (`-race` green, 87.7% coverage). Known contract bug referenced (#59).
- `.env.example` — ORVEXA_OIDC_* security block added (CLIENT_SECRET also keys OAuth
  state HMAC); non-security env drift filed as #82. Worklog conflict block found → #84.
- `docs/security/security-posture.md` — evidence tables + verdict line.

**Verification (CI-equivalent local full matrix):** `gofmt -l .` empty · `go vet ./...`
clean · `go build ./...` clean · `make lint-todos` clean · `go test -race
./internal/platform/httpx/...` green · `go test -race ./...` 31 packages ok / 0 failures.

**PR:** "security: independent sweep — secret scan, govulncheck, headers, rate-limit
matrix (#39)" — Closes #39.

**PR:** "feat(infra): Redis presence cache + distributed rate-limit drivers (#37)" — Closes #37.

**Risks / follow-ups:** glue wave must call `NewPresenceCacheFromEnv` / `NewRateLimitFromEnv`
in `cmd/api` + `httpserver` wiring (the swap points exist and are proven; until then the env var
affects nothing at boot — by design, zero default drift); limiter `scope` values must be chosen
at the mount sites (`auth` vs `webhooks`) with `FailClosed=true` on the auth surface; fixed
window permits up to 2× limit across a bucket boundary (documented; a ZSET sliding window is the
follow-up if that is unacceptable); no Redis Cluster/pub-sub scope by issue constraint.

---

## Wave D2d — [O-31] issue #40: tenancy & authorization matrix (cross-tenant isolation evidence)

**Owner:** Agent D2 (QA engineer, security-adjacent) · **Branch:** `test/authz-matrix` ·
**Predecessor state:** commit 97a3fca (harness booting real stack) + 3 dirty files + untracked
MATRIX.md from two timed-out predecessor sessions; salvaged and pushed.

**Delivered:**
- Salvage (51eb6a6): envelope-aware `jsonField` (unwraps httpx `{data,meta}` before field
  lookup), `to_regclass(...)::text` table probe (pgx 5.7.2 cannot scan OID 2205), interaction
  seeds carrying distinct `ProviderRef` (platform dedupes `(tenant_id, provider, provider_ref)`;
  ref-less binds `''` — defect D7), R27 captures the REAL placed-call id (`telephony.Rec` wire
  key `"ID"`) so R28 holds a simulator-registered leg while the D9 probe uses an unplaced leg;
  `probeForeignList` (D8 ratchet: needle scan + 200-empty-vs-404), `probeDefectMasked`
  (secondary-defect masking: D7's ref-less-create 409 masks D1 on calls/messages), DEF-1 rows
  ratcheting D7, sorted defect registry in the report.
- Matrix run (evidence commit in MATRIX.md): devstack PG (port 55439, migrations applied) +
  real API process (`go run ./cmd/api`, simulator comms, inproc bus, API-key-only auth, search
  degraded) + two tenants/keys minted per the documented bootstrap path (SQL, hashed keys).
  `go test -race -tags=authz ./tests/authz/` → 153 probes, 0 hard failures, 14 DEFECT rows
  (D1–D9 ratchets recorded, never fixed here), 2 PASS(degraded), 2 Conditional.
- Defects filed (failures are NOT fixed in this package): D1→#92, D2→#93, D3→#94, D4→#95,
  D5→#96, D6→#97, D7→#90 (pre-existing O-47, same root cause), D8→#98, D9→#99; registry linked
  and MATRIX.md regenerated (5894802).
- Worklog: resolved the committed `<<<<<<< HEAD / >>>>>>> origin/main` conflict block in this
  file (both predecessor entries kept verbatim), appended this entry.

**Verification (CI-equivalent local full matrix):** `gofmt -l .` empty · `go vet ./...` clean ·
`go build ./...` clean · `go test -race ./...` exit 0 (31 packages ok, 0 FAIL) ·
`go test -race -tags=authz ./tests/authz/` PASS (153 probes, 0 hard failures) ·
`make lint-todos` clean.

**PR:** "test(authz): tenancy & authorization matrix with cross-tenant evidence (#40)" —
Closes #40.

**Risks / follow-ups:** six isolation defects remain open by design (D1–D6 filed as P0 type/bug,
owners assigned per domain; the ratchet rows auto-re-verify as PASS-SECURED once fixed);
identity-plane routes are Conditional (fail-closed 404) until OIDC is provisioned; harness still
boots the API via `go run` (worked cleanly this run — 0.72s suite — but an in-process httptest
assembly is the documented follow-up if it ever proves flaky in CI); search routes are
PASS(degraded) until OpenSearch lands in devstack.

---

## Wave D5e — [O-34]/[O-46] issues #43 + #89: e2e customer-journey suite + e2e-demo.sh upgrade

**Owner:** Agent D5 (Automation/E2E engineer) · **Branch:** `test/e2e-journeys` ·
**Predecessor state:** 3 commits (J1/J2/J3 suite + seed-compile fix) + dirty `scripts/e2e-demo.sh`
rework; this session merged origin/main (P0 tenant isolation #105, Postman #102 — clean merge),
finished, verified and landed the script, and committed evidence.

**Delivered:**
- Journey suite green on post-#105 main (three runs, latest 011544): `go test -race -tags=e2e
  ./tests/e2e/` → J1 inbound WhatsApp (resolve → signed webhook → replay dedupe → tamper
  fail-closed → routing → assignment → wrap-up → case close → conversation close), J2 outbound
  SMS (send → read receipt through the public gateway → tamper/replay injections → analytics),
  J3 AI suggest → allowlisted tool call → 403 policy refusal (audited) → audit_events +
  usage_fact queries. Each journey on its OWN seeded tenant (#90 routing).
- J2 NOT blocked by #103: `messaging.Service.Send` applies queued→active synchronously and the
  read receipt is processed inline by the public webhook handler; the simulator's INTERNAL
  sent/delivered receipts stay ledger-only until #103 (documented in the test's contract notes).
- `scripts/e2e-demo.sh` (#89) landed: step 4 reworked to a supported lifecycle event
  (`message.delivered` for the created inbound interaction — the comms vocabulary has no
  `inbound.whatsapp` and requires interaction_id+tenant_id); jq paths fixed to the real wire
  casing (`data.ID/Status/TenantID`, #101 drift documented, not "fixed"); replay/dedupe +
  tamper assertions kept and extended to the voice leg; MANAGED mode boots devstack(55445) +
  fresh-built api/worker + seeds a fresh tenant (`go run -tags=e2e ./tests/e2e/seed`), runs the
  12-step loop AND the Go journey suite, prints a scored summary; transcripts →
  `qa/evidence/<UTC-date>/journeys/*.txt` (.txt because the repo-wide `*.log` gitignore would
  hide evidence). EXTERNAL mode preserved. `bash scripts/e2e-demo.sh` → exit 0, 12/12 +
  J1/J2/J3 PASS (three consecutive runs).
- Defect filed from the green run (NOT fixed here): #106 — analytics `total_interactions`
  counts lifecycle EVENTS not distinct interactions (one sms send → total=3, bucket `"":2`;
  transition payloads carry no channel). Tenant scoping verified correct.
- Evidence committed: `qa/evidence/2026-09-11/journeys/{demo-loop,go-journeys}-011544.txt`.

**Verification (CI-equivalent local full matrix):** `gofmt -l .` empty · `go vet ./...` clean ·
`go build ./...` clean · `bash -n scripts/e2e-demo.sh` ok · `go test -race ./...` exit 0 ·
`go test -race -tags=e2e ./tests/e2e/` PASS (3 journeys, 0 skips) · `make lint-todos` clean ·
`bash scripts/e2e-demo.sh` exit 0 ×3 (fresh devstack each run).

**PR:** "test(e2e): customer journey suite + e2e-demo upgrade (#43)" — Closes #43, Closes #89.

**Risks / follow-ups:** #101 (interactions wire drift) will change the casing/workarounds the
script and suite intentionally encode — both file the drift in contract notes so the ratchet is
visible when it lands; #103 (simulator internal receipts ledger-only) leaves the demo loop's
voice leg dependent on public-gateway callbacks; #106 inflation keeps J2's analytics assertion
at >=1 (ratchet to ==1 documented); e2e suite still requires the go toolchain + devstack bundle
(no docker) by design.
