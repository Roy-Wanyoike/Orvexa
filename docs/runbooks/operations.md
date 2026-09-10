# Operations Runbook — Orvexa

## Configuration

Copy `.env.example` → `.env`. Every setting has a safe local default; the platform boots with zero external dependencies (in-process bus, simulator carrier, API-key auth, search off, Postgres facts sink). This section documents **every** runtime variable the code reads; the source files are cited so the table can be re-verified against code.

### Core process

| Variable | Default | Notes |
|---|---|---|
| `ORVEXA_HTTP_ADDR` | `:8080` | API listen address (`pkg/config`) |
| `ORVEXA_REALTIME_ADDR` | `:8081` | WebSocket gateway listen address |
| `ORVEXA_DATABASE_URL` | *(empty)* | PostgreSQL. Empty = the API boots degraded (no durable storage; `/readyz` reports the truth); `cmd/worker` and `cmd/realtime` **refuse to start** without it. |
| `ORVEXA_DB_MAX_CONNS` | `20` | pgx pool size |
| `ORVEXA_BUS_DRIVER` | `inproc` | `inproc` \| `nats`. The JetStream driver ships in `internal/platform/bus/nats.go` behind the same `Bus` interface, but `cmd` binaries still boot the in-process bus: with `nats`, `cmd/worker` logs the fallback loudly and continues on in-proc (wiring tracked under roadmap [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10)) |
| `ORVEXA_NATS_URL` | `nats://localhost:4222` | JetStream endpoint; also the gate `bus.NewNATSFromEnv` uses (empty = driver never constructed) |
| `ORVEXA_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `ORVEXA_ENV` | `development` | `production` switches on production posture (`Config.Production()`) |
| `ORVEXA_BOOTSTRAP_API_KEY` | *(empty)* | **Loaded but not consumed by any boot path** — first tenant + key bootstrap is SQL (§ below). Tracked issue [#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56) |
| `ORVEXA_WEBHOOK_HMAC_SECRET` | *(empty)* | **Set in any shared environment.** The platform's HMAC signer + validator: empty secret refuses all webhook ingestion (fail-closed), and the same secret signs every provider webhook the factory loop delivers (real or simulated) |
| `ORVEXA_WS_MAX_PER_PRINCIPAL` / `ORVEXA_WS_MAX_TOTAL` | `10` / `10000` | Realtime connection caps; both must be positive or config load fails |

### Communications providers

Selection is one variable per plane (fail-closed at startup: unknown names, plane mismatches and missing credentials abort the boot with an error that names the offending variables — never their values). Credentials are read **only for the selected provider** and are redacted on every logging/JSON path.

| Variable | Default | Notes |
|---|---|---|
| `ORVEXA_TELEPHONY_PROVIDER` | `simulator` | `simulator` \| `twilio` \| `africastalking` \| `freeswitch` \| `asterisk` (`internal/comms/registry`) |
| `ORVEXA_MESSAGING_PROVIDER` | `simulator` | `simulator` \| `twilio` \| `whatsappcloud` \| `africastalking` |
| `ORVEXA_COMMS_PROVIDER` | `simulator` | **Legacy, superseded** — loaded into config but consumed by nothing since the two-plane registry landed; kept for config compatibility. Use the two variables above |

Per-provider credentials (`internal/comms/registry/doc.go` is the single source of truth; the architecture doc's [provider table](../architecture.md#provider-selection) maps them to adapters):

| Provider | Variables |
|---|---|
| `twilio` (voice + messaging) | `ORVEXA_TWILIO_ACCOUNT_SID`, `ORVEXA_TWILIO_AUTH_TOKEN`, `ORVEXA_TWILIO_FROM_NUMBER` |
| `whatsappcloud` (messaging) | `ORVEXA_WHATSAPP_PHONE_NUMBER_ID`, `ORVEXA_WHATSAPP_ACCESS_TOKEN`, `ORVEXA_WHATSAPP_APP_SECRET`, `ORVEXA_WHATSAPP_VERIFY_TOKEN` |
| `africastalking` (voice) | `ORVEXA_AT_USERNAME`, `ORVEXA_AT_API_KEY`, `ORVEXA_AT_VOICE_PRODUCT_CODE` |
| `africastalking` (messaging) | `ORVEXA_AT_USERNAME`, `ORVEXA_AT_API_KEY`, `ORVEXA_AT_SENDER_ID` (required at boot — fail-closed) |
| `freeswitch` (voice) | `ORVEXA_FREESWITCH_HOST`, `ORVEXA_FREESWITCH_PORT` *(default 8021)*, `ORVEXA_FREESWITCH_PASSWORD` |
| `asterisk` (voice) | `ORVEXA_ASTERISK_HOST`, `ORVEXA_ASTERISK_PORT` *(default 5038)*, `ORVEXA_ASTERISK_USERNAME`, `ORVEXA_ASTERISK_SECRET` |

Unset planes boot the **simulator** and say so: `provider=simulator reason=not_configured`. Twilio deployment endpoints (TwiML/callback/hold document URLs) are application URLs supplied through the factory, not credentials — until provisioned, voice `PlaceCall`/`Hold` fail with typed errors naming the missing knob and messaging sends run fire-and-forget (documented per-adapter postures).

### Infrastructure drivers (all optional; unset = the zero-dependency default)

| Variable | Default | Notes |
|---|---|---|
| `ORVEXA_OPENSEARCH_URL` | *(empty)* | Conversation search backend (issue #36). Empty = search off — the API boots normally and `GET /api/v1/search/conversations` answers the typed 503 `search.not_configured`. A down backend answers 503 `search.unavailable`. Search is a read model and NEVER blocks the interaction hot path. |
| `ORVEXA_OPENSEARCH_INDEX` | `orvexa-conversations` | Index override |
| `ORVEXA_REDIS_URL` | *(empty)* | Shared Redis for the presence cache + distributed rate-limit drivers (issue #37), e.g. `redis://localhost:6379/0`. **Drivers ship as tested library swap points (`NewPresenceCacheFromEnv`, `NewRateLimitFromEnv`); the `cmd` binaries still construct the in-process drivers — wiring is tracked under roadmap [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10).** When wired: presence degrades to DB reads (a Redis error never fails a read); the limiter fails **open** everywhere except auth-critical surfaces, which fail **closed** (429 + `Retry-After`) so an outage cannot lift the bound on protected routes. Malformed URLs are startup errors that do not echo the value. Local dev: `docker compose -f docker-compose.dev.yml up -d redis` |
| `ORVEXA_CLICKHOUSE_URL` | *(empty)* | OLAP facts sink (issue #35). Empty = facts stay on PostgreSQL (byte-identical default); set = the worker's facts consumer batches into ClickHouse (`clickhouse://user:pass@host:9000/db` native or `http://…:8123/db`). An invalid URL aborts the worker at boot (values never echoed) |
| `ORVEXA_CLICKHOUSE_MAX_BATCH_ROWS` | `1000` | Flush trigger across both fact buffers |
| `ORVEXA_CLICKHOUSE_FLUSH_INTERVAL` | `2s` | Background flush cadence (Go duration) |
| `ORVEXA_CLICKHOUSE_WRITE_TIMEOUT` | `5s` | Per-flush timeout (also clamps the dial timeout) |
| `ORVEXA_CLICKHOUSE_ENQUEUE_TIMEOUT` | `5s` | How long a fact may wait for buffer space before the typed `analytics.clickhouse_backpressure` error |
| `ORVEXA_TEMPORAL_URL` | *(empty)* | Temporal frontend `host:port` (gRPC; a pasted `http://`/`https://`/`grpc://` prefix is tolerated and stripped). Only consumed by builds with `-tags=temporal` ([ADR-0009](../adr/0009-temporal-driver.md)); the integration test also gates on it |
| `ORVEXA_TEMPORAL_NAMESPACE` | `default` | Temporal namespace |
| `ORVEXA_TEMPORAL_TASK_QUEUE` | `orvexa-workflows` | Task queue hosting the driver's workflows + activities |
| `ORVEXA_WORKFLOWS_SOURCE` | `orvexa-temporal` | Outbox event source stamped for audit parity |

### Identity (OIDC + capability RBAC)

| Variable | Default | Notes |
|---|---|---|
| `ORVEXA_OIDC_ISSUER` | *(empty)* | IdP discovery root; issuer binding is enforced against the discovery document |
| `ORVEXA_OIDC_CLIENT_ID` | *(empty)* | Required token `aud`; confidential client id |
| `ORVEXA_OIDC_CLIENT_SECRET` | *(empty)* | Code-exchange secret; keys the login-state HMAC. Never logged |
| `ORVEXA_OIDC_REDIRECT_URL` | derived | Fixed callback override; unset = derived from the request host (the IdP's registered-redirect allowlist is the guard) |
| `ORVEXA_OIDC_SCOPES` | `openid` | Comma-separated authorize scopes |

All three core variables must be set for OIDC to be enabled; otherwise the identity plane is disabled and the API-key path is byte-identical to the pre-#38 stack. Full contract: [docs/rbac.md](../rbac.md).

### AI gateway

| Variable | Default | Notes |
|---|---|---|
| `ORVEXA_AI_PROVIDER` | `rules` | `rules` (deterministic, no external calls) \| `llm` (OpenAI-compatible endpoint selection) |
| `ORVEXA_AI_LLM_BASE_URL` / `ORVEXA_AI_LLM_API_KEY` | *(empty)* | LLM endpoint + key, read into config; the shipped `cmd` wiring constructs the rules provider — the LLM adapter is a config-level swap point (roadmap [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10)) |
| `ORVEXA_AI_MAX_TOKENS` | `1024` | Hard cap enforced by the gateway |
| `ORVEXA_AI_TIMEOUT_SECONDS` | `30` | Must be positive or config load fails |

### Test-only and script-scope

| Variable | Scope | Notes |
|---|---|---|
| `ORVEXA_TEST_DATABASE_URL` | `go test -tags=integration` | Integration suites skip cleanly when unset |
| `ORVEXA_URL` / `ORVEXA_KEY` / `ORVEXA_SECRET` | `scripts/e2e-demo.sh` | Operator-supplied shell variables (`:?=` guards); never process config |

## Degradation behaviors

What the platform does when a dependency is missing, down, or slow — and the one thing it never does (block the interaction hot path):

| Dependency | State | Platform behavior |
|---|---|---|
| PostgreSQL | down (API) | Boots; `/readyz` → 503 `degraded`; durable features refuse truthfully. Worker/realtime **fail-fast at boot** (they are consumers; running without storage would lie) |
| PostgreSQL | down (runtime) | Requests touching storage fail with typed errors; health endpoints stay truthful; audit trail intact on recovery |
| OpenSearch | unset | Search route answers 503 `search.not_configured`; indexer stays off; everything else byte-identical |
| OpenSearch | down/slow | Queries → 503 `search.unavailable` (bounded per-request deadline, 3s); the indexer **sheds events** (bounded queue of 1024, drop + log + `Dropped` counter) — publishers are never blocked; facts of truth remain PostgreSQL. Reindex-from-DB is the recovery path |
| Redis | unset | In-process presence TTL store + in-process token bucket (the pre-#37 behavior, byte-identical) |
| Redis | down *(once wired)* | Presence: every read degrades to the DB query (logged, never failed). Limiter: fail **open** elsewhere; auth-critical surfaces fail **closed** (429 + `Retry-After`). No silent in-process fallback limiter — the policy is deliberate and testable |
| NATS / JetStream | driver not wired | `ORVEXA_BUS_DRIVER=nats` → `cmd/worker` logs the fallback and continues on the in-process bus (multi-process fan-out is then unavailable — say so in ops review) |
| NATS / JetStream | down *(once wired)* | Publish is bounded and returns errors — the outbox row stays `pending`/retries; no unbounded buffering; durable consumers resume their cursors on reconnect |
| ClickHouse | down | Facts consumer keeps accepting events into bounded buffers; flushes fail and **requeue rows for retry**; if the outage outlasts buffer capacity the oldest rows are shed (`analytics.buffer_overflow`) — facts are derived data, not the system of record |
| ClickHouse | backpressure | `Handle` blocks up to `ORVEXA_CLICKHOUSE_ENQUEUE_TIMEOUT` then fails typed `analytics.clickhouse_backpressure`; memory is capped by buffer sizes |
| OIDC IdP | unreachable | Discovery retries per request (failures never poison the process); login endpoints answer 503-class `identity.discovery_failed`; already-issued tokens keep verifying against the cached JWKS within TTL |
| OIDC IdP | unset | Identity plane disabled; API-key path unchanged |

## Recovery steps

- **PostgreSQL:** restore from WAL/base backup (§ Backups). The API reattaches on the next request; worker/realtime restart. `outbox_events` rows with `status='failed'` are operator-reviewable — never delete blindly; re-drive after fix. The audit trail is append-only; a restore must preserve it exactly (it is part of the product).
- **OpenSearch:** search self-heals when the backend returns (indexer resumes; in-flight events may have been shed — the `Dropped` counter tells you). For a lost/corrupt index: recreate it, then rebuild from PostgreSQL (source of truth); search will be eventually-consistent until the replay catches up.
- **Redis:** nothing to recover — presence reads hit the DB on every miss/error, and the limiter policy (open/closed) already covered the outage. Re-point `ORVEXA_REDIS_URL`, restart, verify with the Redis driver integration suites.
- **ClickHouse:** restart the server; the store keeps its flush cadence and requeued rows drain once the server answers again. Verify with `clickhouse-client` counts against the worker's flush logs. If rows were shed during a long outage, replay the affected window from retained `outbox_events` through the facts consumer (idempotent by event id; no dedicated replay tool ships yet).
- **NATS / JetStream:** the stream `orvexa-events` and durable consumers re-create/resume on reconnect; if the stream was wiped, the outbox is still the record — re-drive from `outbox_events` (same path as failed-batch recovery).
- **Provider (carrier) outage:** providers deliver lifecycle progress via webhooks; during a carrier outage interactions park in their last lifecycle state and the `provider_events` ledger shows the delivery history. Simulator planes are unaffected (no network I/O).

## Migrations

Forward-only, ordered SQL in `migrations/`:

```bash
make migrations   # requires ORVEXA_DATABASE_URL + psql
```

`db:push`-style destructive flows are deliberately absent; schema changes land as new numbered migrations (latest: `0012_rbac.sql`). The ClickHouse DDL (`0011_clickhouse_facts.sql`) is engine-specific and is NOT applied by the PG runners — the compose path applies it on first init of the ClickHouse volume; existing volumes apply it manually (see `docs/devstack.md`).

## Running

```bash
go run ./cmd/api        # API + webhook ingress + provider factory
go run ./cmd/worker     # outbox dispatcher, audit + facts consumers, search indexer, workflow executor
go run ./cmd/realtime   # WebSocket fan-out
```

Health: `GET /healthz` (liveness, always 200) and `GET /readyz` (component truth; 503 when the DB is down). The realtime gateway exposes `GET /healthz` with the live connection count.

## First tenant bootstrap

1. Insert organization + tenant + API key (SQL until the admin API lands — see roadmap):
   ```sql
   INSERT INTO organizations (id, name, slug) VALUES (gen_random_uuid(), 'Acme', 'acme');
   INSERT INTO tenants (id, organization_id, name) VALUES (gen_random_uuid(), (SELECT id FROM organizations), 'Acme Support');
   -- store only the SHA-256 hash of the raw key:
   INSERT INTO api_keys (id, tenant_id, name, key_hash, scopes)
   VALUES (gen_random_uuid(), (SELECT id FROM tenants), 'bootstrap', digest('<raw-key>','sha256'), '{api}');
   ```
2. Call the API: `curl -H "X-API-Key: <raw-key>" http://localhost:8080/api/v1/`
3. OIDC principals need role bindings (migration `0012`); the principal is the IdP `sub` claim:
   ```sql
   INSERT INTO role_bindings (id, tenant_id, principal, role, granted_by)
   VALUES (gen_random_uuid(), (SELECT id FROM tenants), '<idp-sub>', 'admin', 'bootstrap-sql');
   ```
   Revoking is an UPDATE to `status='revoked'` — capabilities drop within the 30s resolver TTL, even while tokens stay cryptographically valid.

## Webhook signatures

The gateway is fail-closed: nothing unverified is ever persisted as processed, all MAC comparisons are constant-time, and rejection messages are fixed strings (no secret material in errors or logs).

| Sender | Verification |
|---|---|
| Simulator / internal provider loop | `X-Orvexa-Signature: hex(hmac_sha256(ORVEXA_WEBHOOK_HMAC_SECRET, body))` — replay deliveries return the original event id (idempotent) |
| `twilio` | `X-Twilio-Signature: base64(HMAC-SHA1(authToken, signedPayload))` over the composed URL + POST fields; a token-rotation list keeps two auth tokens valid simultaneously |
| `whatsappcloud` | `X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(appSecret, rawBody))>`; the legacy SHA-1 header is never trusted |
| `africastalking` | No documented signature scheme — verification is a server-side allowlist of edge-controlled peer addresses (trusted-proxy header semantics documented in `internal/webhooks/verify_africastalking.go`); empty allowlist = fail-closed |

Unsigned or tampered bodies are rejected 401 and never persisted as processed.

## Backups & recovery

The database is the system of record (interactions, audit, workflow state). Recommended posture:

- PostgreSQL continuous archiving (WAL) + nightly base backup; restore drills quarterly.
- `outbox_events` rows with `status='failed'` are operator-reviewable — never delete blindly; re-drive after fix.
- The audit trail is append-only; restore must preserve it exactly (it is part of the product).
- Search and ClickHouse facts are derived read models — rebuildable from PostgreSQL and retained outbox events respectively (see § Recovery steps).

## Deployment posture (production)

- Terminate TLS at the edge; the edge proxy MUST overwrite `X-Forwarded-For` from the socket address (rate limiting keys on the direct peer — and the Africa's Talking verifier's allowlist consumes that header).
- Expose the API and realtime gateway only via the proxy; never expose :8080/:8081 directly.
- Set `ORVEXA_WEBHOOK_HMAC_SECRET` per environment; rotate by double-reading during the transition (the Twilio verifier natively supports a two-token rotation window).
- Scale `cmd/worker` replicas freely — dispatcher and workflow claims are SKIP-LOCKED safe; consumers are idempotent.
- Multi-replica `cmd/api`: presence sharing and global rate limits require the Redis drivers to be wired into the binaries (drivers shipped; wiring tracked under [#10](https://github.com/Roy-Wanyoike/Orvexa/issues/10)). Once wired: node clocks must be NTP-synced — the Redis limiter derives its window buckets from the caller's clock so every node lands on the same bucket.
- Shipped and production-ready behind config: real carrier adapters (Twilio voice/SMS, WhatsApp Cloud, Africa's Talking voice/SMS+USSD, FreeSWITCH ESL, Asterisk AMI), the OIDC identity plane + capability RBAC, the ClickHouse facts sink, the OpenSearch search plane. Library-complete pending `cmd` wiring: the NATS JetStream bus driver and the Redis presence/limiter drivers (both env-gated swap points, integration-tested).
- `ORVEXA_BOOTSTRAP_API_KEY` remains a dead knob until [#56](https://github.com/Roy-Wanyoike/Orvexa/issues/56) decides implement-or-remove; bootstrap is SQL today.
