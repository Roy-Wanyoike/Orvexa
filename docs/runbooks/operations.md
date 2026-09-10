# Operations Runbook — Orvexa

## Configuration

Copy `.env.example` → `.env`. Every setting has a safe local default; the platform boots with zero external dependencies (in-process bus, simulator carrier).

| Variable | Default | Notes |
|---|---|---|
| `ORVEXA_HTTP_ADDR` | `:8080` | API listen address |
| `ORVEXA_REALTIME_ADDR` | `:8081` | WebSocket gateway |
| `ORVEXA_DATABASE_URL` | *(empty)* | PostgreSQL. Empty = degraded boot (no durable storage; readiness reports truth). Required for worker + realtime. |
| `ORVEXA_BUS_DRIVER` | `inproc` | `nats` requires the NATS driver (tracked issue O-10) |
| `ORVEXA_NATS_URL` | `nats://localhost:4222` | JetStream subjects when `ORVEXA_BUS_DRIVER=nats` |
| `ORVEXA_OPENSEARCH_URL` | *(empty)* | Conversation search backend (issue #36). Empty = search off — the API boots normally and `GET /api/v1/search/conversations` answers the typed 503 `search.not_configured`. A down backend answers 503 `search.unavailable`; search is a read model and NEVER blocks the interaction hot path (the indexer sheds events instead of slowing publishers). `ORVEXA_OPENSEARCH_INDEX` overrides the default `orvexa-conversations`. |
| `ORVEXA_REDIS_URL` | *(empty)* | Shared Redis for the presence cache + rate-limit drivers (issue #37), e.g. `redis://localhost:6379/0`. Empty = the in-process drivers, byte-identical to the pre-#37 behavior. Set = agent presence is cached in Redis with a short TTL (DB stays the source of truth; invalidation is explicit and shared across nodes) and rate limits become GLOBAL per tenant + route class instead of per instance. Outage semantics: presence NEVER fails a read — a Redis error degrades to the DB read (logged); the limiter applies its configured policy — fail **open** everywhere except auth-critical surfaces (the authenticated API), which fail **closed** (429 + `Retry-After`) so a Redis outage cannot lift the bound on protected routes. Local dev: `docker compose -f docker-compose.dev.yml up -d redis`; integration suites: `ORVEXA_REDIS_URL=... go test -race -tags=integration ./internal/routing/ ./internal/platform/httpx/`. |
| `ORVEXA_WEBHOOK_HMAC_SECRET` | *(empty)* | **Set in any shared environment.** Empty secret refuses all webhook ingestion (fail-closed). |
| `ORVEXA_COMMS_PROVIDER` | `simulator` | simulator performs no network I/O and signs payloads like a real carrier |
| `ORVEXA_AI_PROVIDER` | `rules` | deterministic provider; LLM endpoints are config-level adapters |
| `ORVEXA_AI_MAX_TOKENS` / `ORVEXA_AI_TIMEOUT_SECONDS` | 1024 / 30 | hard caps enforced by the gateway |
| `ORVEXA_WS_MAX_PER_PRINCIPAL` / `ORVEXA_WS_MAX_TOTAL` | 10 / 10000 | connection caps |

## Migrations

Forward-only, ordered SQL in `migrations/`:

```bash
make migrations   # requires ORVEXA_DATABASE_URL + psql
```

`db:push`-style destructive flows are deliberately absent; schema changes land as new numbered migrations.

## Running

```bash
go run ./cmd/api        # API + webhook ingress
go run ./cmd/worker     # outbox dispatcher, audit + analytics consumers, workflow executor
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

## Webhook signatures

Providers (and the simulator) POST raw JSON with `X-Orvexa-Signature: hex(hmac_sha256(secret, body))`. Unsigned or tampered bodies are rejected 401 and never persisted as processed. Replay deliveries return the original event id (idempotent).

## Backups & recovery

The database is the system of record (interactions, audit, workflow state). Recommended posture:

- PostgreSQL continuous archiving (WAL) + nightly base backup; restore drills quarterly.
- `outbox_events` rows with `status='failed'` are operator-reviewable — never delete blindly; re-drive after fix.
- The audit trail is append-only; restore must preserve it exactly (it is part of the product).

## Deployment posture (production)

- Terminate TLS at the edge; the edge proxy MUST overwrite `X-Forwarded-For` from the socket address (rate limiting keys on the direct peer).
- Expose the API and realtime gateway only via the proxy; never expose :8080/:8081 directly.
- Set `ORVEXA_WEBHOOK_HMAC_SECRET` per environment; rotate by double-reading during the transition (adapter-level concern for real carriers).
- Scale `cmd/worker` replicas freely — dispatcher and workflow claims are SKIP-LOCKED safe; consumers are idempotent.
- Scale `cmd/api` replicas freely once `ORVEXA_REDIS_URL` points at a shared Redis (issue #37): agent presence becomes a shared TTL cache (DB truth + explicit invalidation across nodes) and API rate limits become global per tenant + route class instead of per instance. Node clocks must be NTP-synced — the limiter derives its window buckets from the caller's clock so every node lands on the same bucket. Before that variable is set, each API replica limits independently (per-instance budgets).
- Real carrier adapters (Twilio, WhatsApp Cloud, Africa's Talking), the NATS JetStream driver, ClickHouse facts store, OpenSearch indexer, Redis presence and OIDC/RBAC are tracked under roadmap issue [#10](../../issues/10) with per-adapter acceptance criteria.
