# Orvexa Devstack — portable PostgreSQL 16 + one-command integration suite

Issue: [#23](https://github.com/Roy-Wanyoike/Orvexa/issues/23) — [O-14].

The integration suite (`tests/integration`, build tag `integration`) is gated on
`ORVEXA_TEST_DATABASE_URL` and needs a real PostgreSQL 16. This devstack makes
that a one-command reality in two flavors:

| Path | Requires | Entry point |
| --- | --- | --- |
| **Userland (default)** | bash, curl, unzip, tar, xz — **no docker, no sudo** | `scripts/devstack.sh` / `make integration` |
| **Docker compose** | docker + compose plugin | `docker-compose.dev.yml` |

---

## Path A — userland devstack (no docker, no sudo)

PostgreSQL 16 runs from your home directory. Binaries come from the pinned
Zonky `embedded-postgres-binaries-linux-amd64` Maven artifact (a ZIP whose
`embedded-postgres-binaries-*.txz` contains the server tree — `bin/postgres`,
`bin/initdb`, `bin/pg_ctl`, `share/`, `lib/`).

> **Note:** the Zonky server bundle deliberately ships no client tools (no
> `psql`, no `pg_isready`). The devstack therefore (a) gates readiness with a
> real pgx wire-protocol handshake (strictly stronger than a TCP ping) and
> (b) applies migrations with a small pgx runner compiled via `go run` from the
> module's existing dependencies (`GOFLAGS=-mod=readonly`; `go.mod`/`go.sum`
> are never modified). If a system `psql` exists it is used when you invoke
> `devstack.sh psql` explicitly.

### Quickstart

```bash
scripts/devstack.sh init     # download bundle (cached) + initdb
scripts/devstack.sh start    # idempotent; starts server, applies migrations
make integration             # runs go test -race -tags=integration ./...
scripts/devstack.sh stop
```

`make integration` auto-starts the devstack when `ORVEXA_TEST_DATABASE_URL` is
unset — `make integration` alone is enough on a fresh clone.

### Subcommands

| Command | Behavior |
| --- | --- |
| `init` | Resolve + download Zonky bundle (sha256-verified, cached), `initdb`, write config. Re-run safe. |
| `start` | Idempotent. Runs `init` if needed, starts postgres, waits for a real SQL handshake (pgx probe), auto-applies `migrations/*.sql`. Already-running → no-op. |
| `stop` | Clean shutdown: `pg_ctl -m smart`, escalating to `fast`, then `immediate` (each logged). Removes pid file. |
| `status` | `RUNNING pid=… port=…` or `STOPPED`. |
| `psql` | Open a SQL shell: bundle psql if present, else system psql, else a loud error pointing at `devstack.sh url`. |
| `url` | Prints `postgres://postgres:postgres@127.0.0.1:55432/orvexa?sslmode=disable`. |
| `clean` | Stop + remove data/log/pid state. Downloads cache kept (offline re-init). `clean --all` also wipes the cache. |

### Configuration (environment)

| Variable | Default | Notes |
| --- | --- | --- |
| `ORVEXA_DEVSTACK_PORT` | `55432` | Server port |
| `ORVEXA_DEVSTACK_HOST` | `127.0.0.1` | **Keep loopback.** The stack binds only this address |
| `ORVEXA_DEVSTACK_USER` / `_PASSWORD` / `_DB` | `postgres` / `postgres` / `orvexa` | Dev credentials, scram-sha-256 |
| `ORVEXA_TEST_DATABASE_URL` | — | When set, `devstack.sh url` and `make integration` use it **verbatim** (external DB / CI override; devstack is not started) |

### Layout (all gitignored under `.devstack/`)

```
.devstack/
├── cache/zonky/          # downloaded .jar + recorded .sha256 (offline re-runs)
├── bundle/<version>/     # extracted bin/, share/, lib/
├── pg/data/              # PGDATA (disposable, fsync=off for dev speed)
├── run/                  # unix sockets, initdb scratch
├── log/                  # postgres.log, initdb.log, devstack.log
├── state.env             # resolved version + port (written at init)
├── migrations-applied.log # "<RFC3339 UTC> <file>" marker per successful apply
└── devstack.pid          # postmaster pid (removed on stop)
```

### Migrations

On every successful `start` (and therefore on every `make integration` run),
`migrations/*.sql` are applied in **lexical order** with `ON_ERROR_STOP=1`
(psql) or in a transaction per file (fallback runner), tracked in a
`schema_migrations(version, applied_at)` table — already-applied files are
skipped, a failing migration aborts loudly and is recorded only on success.
Every successful apply also appends a `<RFC3339 UTC> <file>` marker line to
`.devstack/migrations-applied.log` (audit trail; wiped by `clean`).

### Artifact pinning & verification

Pinned artifact (exact version, verified sha256 on every download):

```
https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/16.4.0/embedded-postgres-binaries-linux-amd64-16.4.0.jar
sha256 = 14a5cf546aee7d327a2f5b46be6c571f2f724a2b485c270d46f3e44a1ac3df18
```

The script verifies the download against **both** the constant pinned in
`scripts/devstack.sh` (`ZONKY_SHA256_PINNED`) and the `.sha256` served by Maven
Central; a mismatch aborts before anything is executed. To verify by hand:

```bash
curl -fsSL "<artifact-url>" | sha256sum
curl -fsSL "<artifact-url>.sha256"
```

If the pinned version ever disappears from Maven (HTTP 404 — transient network
errors do **not** trigger this), the script resolves the closest 16.x from
`maven-metadata.xml`, logs `FALLBACK VERSION SELECTED: <ver>` and records the
resolved version in `.devstack/state.env`. Update the pin in a follow-up PR.

## Path B — docker compose (docker-capable environments only)

```bash
docker compose -f docker-compose.dev.yml up -d          # postgres:16 + redis:7 + nats:2.10-jetstream
# optional, env-gated engines (inert until their ORVEXA_*_URL is set):
#   clickhouse:24 (issue #35, see below) / opensearch:2 (issue #36)
ORVEXA_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/orvexa?sslmode=disable' make integration
docker compose -f docker-compose.dev.yml down           # add -v to drop data
```

Same env contract and the same host port (55432) as the userland path, so
tooling is identical. Migrations are applied by the authoritative lexical-order
path (`make migrations` or the devstack), not by container entrypoints.

## ClickHouse facts engine (issue #35, optional)

The analytics facts pipeline has an optional OLAP backend ([O-26]): with
`ORVEXA_CLICKHOUSE_URL` **unset or empty** the worker keeps appending facts to
PostgreSQL (`internal/analytics`, migration 0010) — byte-identical fallback,
zero drift. When the URL is set, the batched, backpressure-safe store in
`internal/analytics/clickhouse` appends there instead (never both).

### Compose path (dev)

```bash
docker compose -f docker-compose.dev.yml up -d clickhouse
# Gate contract consumed by the analytics worker's facts-store selection
# (clickhouse.FromEnv): set → ClickHouse store, unset → PostgreSQL path.
ORVEXA_CLICKHOUSE_URL=clickhouse://default@127.0.0.1:19000/default
```

> **Wiring note:** `clickhouse.FromEnv` (internal/analytics/clickhouse) is the
> selection point the worker wires behind `ORVEXA_CLICKHOUSE_URL`; the store,
> gating and tests live in the package. Docker-free machines apply the DDL
> manually (command below) instead of via compose init.

| Aspect | Value |
| --- | --- |
| Image / container | `clickhouse/clickhouse-server:24` / `orvexa-dev-clickhouse` |
| Host ports | `19000` (native, clickhouse-go) · `18123` (HTTP) — non-standard like PG's `55432`, a local engine keeps `9000`/`8123` |
| DDL | `migrations/0011_clickhouse_facts.sql` is mounted into `/docker-entrypoint-initdb.d/` — applied on **first init** of the `orvexa-dev-clickhouse` volume only |
| Wipe | `docker compose -f docker-compose.dev.yml down -v` drops facts; DDL re-applies on the next first-init |
| Auth | Dev default (no password), loopback posture — same as the stack's `redis`/`nats` |

### Engine-specific migration (0011) — why the PG runners skip it

`migrations/0011_clickhouse_facts.sql` is **ClickHouse DDL, not PostgreSQL**.
Both PostgreSQL runners skip every `migrations/*clickhouse*.sql` file by name
convention (`make migrations` and both `scripts/devstack.sh` runners print an
explicit skip line). Apply it manually to an existing server/volume:

```bash
clickhouse-client --multiquery --host 127.0.0.1 --port 9000 \
  < migrations/0011_clickhouse_facts.sql
```

Schema in one line: `interaction_fact` / `usage_fact` (event type = target
table, mirroring the Postgres consumer's topic routing), ReplacingMergeTree
engines keyed `(tenant_id, toStartOfHour(occurred_at), event_id)` — per-tenant
hourly dashboard layout, with `event_id` as the uniqueness tiebreaker so FINAL
collapses redeliveries only (at-least-once bus delivery) — monthly partitions,
`DateTime64(3,'UTC')` for TIMESTAMPTZ parity.

### Retention notes

- Retention is enforced **server-side**: `TTL occurred_at + INTERVAL 13 MONTH`
  (a full trailing year of dashboards plus slack) — no purge job exists to
  forget. Monthly partitions make eviction part-level and cheap.
- 13 months is a starting point; when a formal retention policy lands, adjust
  the `INTERVAL` in 0011 (and `ALTER TABLE ... MODIFY TTL` on live volumes).
- Strict reads over facts (dedup visibility) must use `FINAL` — see the
  store's integration test for the canonical query shapes.

### Tuning knobs (all optional; safe defaults)

| Variable | Default | Meaning |
| --- | --- | --- |
| `ORVEXA_CLICKHOUSE_URL` | *(unset)* | Gate + DSN (`clickhouse://…:9000/db` native or `http://…:8123/db`) |
| `ORVEXA_CLICKHOUSE_MAX_BATCH_ROWS` | `1000` | Buffered rows before a flush |
| `ORVEXA_CLICKHOUSE_FLUSH_INTERVAL` | `2s` | Background flush cadence |
| `ORVEXA_CLICKHOUSE_WRITE_TIMEOUT` | `5s` | Per-flush (and dial) timeout |
| `ORVEXA_CLICKHOUSE_ENQUEUE_TIMEOUT` | `5s` | How long `Handle` blocks when the bounded buffer is full before failing with `analytics.clickhouse_backpressure` |

### Integration evidence

```bash
ORVEXA_TEST_CLICKHOUSE_URL=clickhouse://default@127.0.0.1:19000/default \
  go test -race -tags=integration -v ./internal/analytics/clickhouse
```

The test skips cleanly when neither env var is set. It re-applies the
idempotent 0011 DDL, streams facts plus a byte-identical redelivery through
the real batching path, and asserts FINAL dedup + aggregation shape + amounts
against the live engine (evidence logged with `t.Logf`).

## How the integration suite consumes the devstack

`tests/integration/integration_test.go` (build tag `integration`) skips unless
`ORVEXA_TEST_DATABASE_URL` is set; with it set, the suite connects with
`pgxpool` and exercises the real DB-backed paths (webhook ingest dedup,
signature rejection, outbox dispatch). `make integration` wires the URL and
runs `go test -race -tags=integration ./...`.

Note: the suite's fixtures assume a **fresh database** (e.g. the dedup test
asserts its first delivery is not a duplicate). Re-runs on a dirty database can
fail on fixture state, not code — reset with
`scripts/devstack.sh clean && scripts/devstack.sh start` (or
`docker compose -f docker-compose.dev.yml down -v && up -d`) before re-running.

## Troubleshooting

| Symptom | Cause / fix |
| --- | --- |
| `port ... is already in use` | Another postgres (or the compose stack) owns the port. `ORVEXA_DEVSTACK_PORT=55433 scripts/devstack.sh start` or stop the other stack. |
| `devstack: STOPPED` but stale pid file exists | Left over from a crash — `start` clears it automatically; `stop` is a no-op. |
| `postgres not ready after 60s` | Inspect `.devstack/log/postgres.log` (and `initdb.log` for bootstrap failures). |
| `sha256 MISMATCH` | Artifact tampered or corrupted download; delete `.devstack/cache/zonky/` and re-run; investigate if it persists. |
| Offline re-run fails | First run needs the network. Afterwards the cache under `.devstack/cache/zonky/` serves re-inits; never run `clean --all` on an offline box. |
| `go` missing + no psql | Readiness/migrations use a small pgx runner compiled from the module's deps (`go build`, `GOFLAGS=-mod=readonly`); without `go` and without a system `psql` the stack cannot gate readiness — install go (userland is fine) or psql. |
| Want a fresh database | `scripts/devstack.sh clean && scripts/devstack.sh start` (re-applies migrations from scratch). |
| Verify no orphan processes after stop | `pgrep -af 'postgres.*devstack'` must be empty. |

## Security notes

- Server binds `127.0.0.1` only; host auth is `scram-sha-256`; credentials are
  dev defaults and are never written to logs (URLs are masked as `postgres:***@`).
- The only fetched code is the pinned Maven artifact above (sha256-verified);
  the devstack never pipes remote scripts into a shell.
- `fsync/synchronous_commit/full_page_writes` are disabled **for dev speed**;
  this stack is disposable by design and must never back real data.

## Rollback

Everything is additive: delete `.devstack/`, drop the new Makefile targets,
and remove `scripts/devstack.sh` / `docker-compose.dev.yml` / `docs/devstack.md`.
No production DB, API, or schema surface is touched.
