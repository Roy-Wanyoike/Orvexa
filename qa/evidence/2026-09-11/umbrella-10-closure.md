# Umbrella #10 closure evidence — 2026-09-11 (Wave E4, issue #51)

Repo state verified: `main` @ `283aed5b` (post-#112), PRs cross-checked via REST API
(`merged_at` timestamps below are from the GitHub API, not inferred). All conformance
suites re-run on this exact tree with `go test -race -count=1`; raw output in
`12-carrier-conformance.txt` and `13-driver-planes.txt` (same directory).

## Per-item evidence table

| # | Item (sub-issue) | PR(s) | Merged | Evidence | Status |
|---|------------------|-------|--------|----------|--------|
| 1 | Twilio Voice ([O-16], #25) | #70 | 2026-09-10T19:51:36Z | PR body: conformance kit PASS, fake-carrier lifecycle e2e, error/retry/redaction tables. Re-run @ HEAD: `TestTwilioVoiceConformance` PASS (4.67s) | ✅ LANDED |
| 2 | Twilio SMS ([O-17], #26) | #64 | 2026-09-10T19:50:45Z | PR body: `--- PASS: TestTwilioSMSConformance (4.04s)` with RequireAsyncEvents + EnforceSendValidation. Re-run @ HEAD: PASS (4.06s) | ✅ LANDED |
| 3 | WhatsApp Cloud ([O-18], #27) | #65 | 2026-09-10T19:50:49Z | PR body: conformance PASS, all 6 scenarios (incl. 4096-multibyte boundary), 122 test runs incl. `-count=10` stability. Re-run @ HEAD: `TestWhatsAppCloudMessagingConformance` PASS (4.04s) | ✅ LANDED |
| 4 | Africa's Talking Voice ([O-19], #28) | #69 | 2026-09-10T19:51:05Z | PR body: conformance kit PASS (async mode), fake-carrier lifecycle, error table, redaction sweep. Re-run @ HEAD: `TestATVoiceConformance` PASS (9.09s) | ✅ LANDED |
| 5 | Africa's Talking SMS + USSD ([O-20], #29) | #68 | 2026-09-10T19:51:01Z | PR body: `TestATSMSConformance` (EnforceSendValidation, sync-102 contract) + signed ProviderEvent receipts + USSD session suite (CON/END, retry idempotency, race probe). Re-run @ HEAD: PASS (0.01s) + USSD suite green | ✅ LANDED |
| 6 | FreeSWITCH ESL ([O-21], #30) | #66 | 2026-09-10T19:50:53Z | PR body: hand-rolled ESL codec w/ golden wire tests, auth handshake variants, reconnect ladder, fake ESL switch. Re-run @ HEAD: `TestVoiceConformance` PASS (18.12s, real TCP) | ✅ LANDED |
| 7 | Asterisk AMI ([O-22], #31) | #67 | 2026-09-10T19:50:57Z | PR body: full `RunVoiceConformance` over real TCP run twice (channel vs Newchannel-only binding), RequireAsyncEvents + EnforceCommandValidation, fake AMI parses client traffic with production codec. Re-run @ HEAD: both PASS (27.05s / 27.06s) | ✅ LANDED |
| 8 | Provider factory wiring ([O-23], #32) | #71 | 2026-09-10T20:57:46Z | Wiring table test: every provider constructs; misconfig names env var; unset → simulator with honest `reason=not_configured` logging. **Wired in `cmd/api` (`factory.Build`, `registry.LoadFromEnv`)** | ✅ LANDED + WIRED |
| 9 | NATS JetStream bus driver ([O-24], #33) | #72 | 2026-09-10T20:57:49Z | Bus conformance kit: InProc reference + NATS-fake parity green; `-tags=integration` suite (kit + ack/nak redelivery + durable-cursor restart) compiles and skips cleanly; live-server repro documented. `NewNATSFromEnv` swap point shipped. **Wire status: library swap point — cmd/worker still boots InProc and logs loud fallback when `ORVEXA_BUS_DRIVER=nats` (honest, documented in runbook/architecture)** → follow-up #113 | ✅ LANDED (wiring follow-up #113) |
| 10 | Temporal workflow driver ([O-25], #34) | #73, #78, #79 | 22:59:24Z / 22:33:23Z / 22:34:50Z | Build-tagged (`temporal`): default builds byte-identical (no SDK import); unit tests over client fakes; integration gated on `ORVEXA_TEMPORAL_URL`; ADR-0009 records per-deployment wiring decision. Re-run @ HEAD: `go test -race ./internal/workflows/...` green in default AND `-tags=temporal` (temporaldriver 1.106s); `go vet -tags=integration,temporal` clean | ✅ LANDED |
| 11 | ClickHouse facts writer ([O-26], #35) | #76, #80 | 22:35:28Z / 22:38:08Z | Batched non-blocking store, env-gated; compose `clickhouse:24` + `0011_clickhouse_facts.sql` DDL init; **wired in `cmd/worker`** — sink selection: ClickHouse when `ORVEXA_CLICKHOUSE_URL` set, PG fallback, misconfig exits with typed error. Re-run @ HEAD: `internal/analytics/clickhouse` ok | ✅ LANDED + WIRED |
| 12 | OpenSearch search ([O-27], #36) | #74, #80 | 21:29:48Z / 22:38:08Z | Indexer consumer + tenant-scoped `GET /search/conversations`; **wired in `cmd/api`** (`search.NewServiceFromEnv` + #80 route mount inside authed v1 group); degradation: unset → 503 `search.not_configured`, backend down → 503 `search.unavailable`, principal-less → 401; compose `opensearch:2` service. Re-run @ HEAD: `internal/search` ok | ✅ LANDED + WIRED |
| 13 | Redis presence + rate-limit drivers ([O-28], #37) | #75 | 2026-09-10T21:57:08Z | Real Redis 7.2.5 integration: `TestRedisPresenceComposeIntegration` PASS (1.10s), `TestRedisRateLimitComposeIntegration` PASS (2.16s, two clients share bucket); swap equivalence vs in-process token bucket proven; key/credential non-logging battery. `NewPresenceCacheFromEnv` / `NewRateLimitFromEnv` swap points shipped. **Wire status: library swap points — cmd/api still boots in-process presence store + rate limiter (honest, documented)** → follow-up #113 | ✅ LANDED (wiring follow-up #113) |
| 14 | OIDC/OAuth2 + capability RBAC ([O-29], #38) | #77 | 2026-09-10T22:30:20Z | Dual-auth branching: identity plane auto-constructs from `ORVEXA_OIDC_*` inside `httpserver.New` when env present (API-key-only otherwise); JWKS floor, resolver TTL, 4096 token cap, capability catalog, error taxonomy; `docs/rbac.md` + ADR-0008. Re-run @ HEAD: `internal/identity` ok (3.03s) | ✅ LANDED + WIRED |

## Supporting infrastructure

| Item (sub-issue) | PR(s) | Merged | Evidence | Status |
|------------------|-------|--------|----------|--------|
| Conformance test-kit (#21) | #60 | 2026-09-10T17:42:07Z | `RunVoiceConformance`/`RunMessagingConformance` + 5 negative-control broken adapters failing the kit for designed reasons (kit self-audit). Re-run @ HEAD: `internal/comms/conformance` ok (3.90s) | ✅ LANDED |
| Provider registry + credentials (#22) | #61 | 2026-09-10T17:42:11Z | Per-provider `Secret`-typed credential config, redaction guarantees, env-only names; wired via `registry.LoadFromEnv` in cmd/api. Re-run @ HEAD: `internal/comms/registry` ok | ✅ LANDED |
| Devstack (#23) | #62 | 2026-09-10T17:42:15Z | `scripts/devstack.sh` + Makefile targets + compose; integration suite green (see `05-devstack.txt`/`06-integration.txt` from #112) | ✅ LANDED |
| Webhook signature verifiers (#24) | #63 | 2026-09-10T17:42:20Z | Twilio X-Twilio-Signature / WhatsApp X-Hub-Signature-256 / AT allowlist+peer-IP; attack matrix in test output; fail-closed gateway path shared by all adapters | ✅ LANDED |
| Glue: search mount + CH sink selection (#35/#36 wiring) | #80 | 2026-09-10T22:38:08Z | Route mount + env-selected facts sink; 31 pkgs race-green per PR body | ✅ LANDED |
| Release-gate fixes / deps (#87) | #87 | 2026-09-10T23:14:06Z | pgx 5.11.0 + chi 5.3.2, replay-id contract, CI pin, dead knob removal; govulncheck: 0 reachable | ✅ LANDED |

## Conformance re-run (this tree, 2026-09-11)

`go test -race -count=1 ./internal/telephony/... ./internal/messaging/... ./internal/comms/...`
→ **12 packages ok, 0 fail** (~2 min; raw output `12-carrier-conformance.txt`). All 8 embedded
conformance kit runs PASS (7 carrier adapters; Asterisk run twice per binding mode) + kit
self-audit (negative controls) PASS.

Driver planes re-run: bus / clickhouse / search / routing / httpx / identity / workflows
green in default mode; `internal/workflows/temporaldriver` green under `-tags=temporal`;
`go vet -tags=integration,temporal` clean (`13-driver-planes.txt`).

Gates on this tree: `go build ./...` clean · `go vet ./...` clean · `gofmt -l .` empty.

## Residual-check notes

- **#103 (simulator receipts consumer)**: RESOLVED — PR #109 merged 2026-09-11T02:00:20Z
  (`e97351b9` in main); #103 closed. No follow-up needed.
- **GAP-1 (P3 follow-up: #113)**: NATS JetStream bus driver + Redis presence/limiter drivers
  ship as tested library swap points (`NewNATSFromEnv`, `NewPresenceCacheFromEnv`,
  `NewRateLimitFromEnv`) but the cmd binaries still boot the InProc bus and in-process
  presence/limiter. This is stated honestly in the runbook degradation table and
  architecture doc — not a silent gap. Tracked as [#113](https://github.com/Roy-Wanyoike/Orvexa/issues/113).
- **Temporal** is build-tagged and constructed per deployment by design (ADR-0009); not
  counted as a gap.
- Umbrella acceptance criteria: env-only credentials (registry/FromEnv constructors) ✅ ·
  health checks (healthz/readyz + per-dependency checks) ✅ · graceful degradation
  (runbook degradation table; search 503 taxonomy; bus fallback logging) ✅.

## Verdict

Every line item of umbrella #10 is evidenced by a merged PR with in-body test evidence and
re-verified green at HEAD. #10 is closed with this evidence; the single residual (NATS/Redis
cmd wiring) is filed as a P3 follow-up ([#113](https://github.com/Roy-Wanyoike/Orvexa/issues/113)) so nothing disappears into limbo.
