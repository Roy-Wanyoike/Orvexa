-- Orvexa 0011 — ENGINE-SPECIFIC MIGRATION: ClickHouse (issue #35, [O-26]).
--
-- This file is NOT PostgreSQL DDL and is NOT applied by the PostgreSQL
-- migration runners: `make migrations` and scripts/devstack.sh skip every
-- migrations/*clickhouse*.sql file by name convention. It is applied to the
-- ClickHouse engine instead:
--   - automatically, on first init, by the compose devstack (the clickhouse
--     service mounts this file into /docker-entrypoint-initdb.d/ — see
--     docker-compose.dev.yml), or
--   - manually, for an existing server/volume:
--       clickhouse-client --multiquery < migrations/0011_clickhouse_facts.sql
--
-- Why ClickHouse: analytics facts are append-heavy and scan-heavy; hosting
-- dashboards on the transactional store couples OLAP load to call handling
-- (critical-path principle). Postgres keeps 0010's fact tables for the
-- fallback path; when ORVEXA_CLICKHOUSE_URL is set the analytics worker
-- appends here instead (internal/analytics/clickhouse).
--
-- Event type is encoded by the target table, mirroring the Postgres
-- consumer's topic routing (internal/analytics/service.go):
--   interaction.created/updated/completed → interaction_fact
--   usage.recorded                        → usage_fact
--
-- Schema mirrors migrations/0010_workflows_analytics.sql:
--   event_id      — envelope id; redeliveries carry the identical row, so
--                   ReplacingMergeTree eventually collapses duplicates
--                   (query with FINAL for strict correctness).
--   tenant_id     — tenant scoping; first key column so per-tenant scans prune.
--   occurred_at   — envelope time (DateTime64(3) for millisecond parity with
--                   TIMESTAMPTZ), monthly partitions power retention + pruning.
--   channel/direction/status/metric — LowCardinality(String) dictionaries;
--                   they are the Summary aggregation's group-by columns.
--
-- Sort key (tenant_id, toStartOfHour(occurred_at), event_id): per-tenant data
-- is clustered into hourly buckets — the layout dashboards read. event_id is
-- appended as the uniqueness tiebreaker: with the bare (tenant_id,
-- toStartOfHour(ts)) key, ReplacingMergeTree would collapse ALL distinct
-- events sharing a tenant-hour, not just redeliveries. With event_id in the
-- key, FINAL dedupes redeliveries and never merges distinct events.
--
-- Retention: 13 months (a full year of trailing-month dashboards plus
-- slack), enforced server-side by TTL so no purge job exists to forget.
-- Monthly partitions make TTL eviction part-level (cheap, precise). Adjust
-- the INTERVAL when retention policy lands.

CREATE TABLE IF NOT EXISTS interaction_fact
(
    event_id       UUID,
    tenant_id      UUID,
    interaction_id UUID,
    channel        LowCardinality(String),
    direction      LowCardinality(String),
    status         LowCardinality(String),
    occurred_at    DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant_id, toStartOfHour(occurred_at), event_id)
TTL occurred_at + INTERVAL 13 MONTH
SETTINGS index_granularity = 8192;

CREATE TABLE IF NOT EXISTS usage_fact
(
    event_id    UUID,
    tenant_id   UUID,
    metric      LowCardinality(String),
    amount      Int64,
    occurred_at DateTime64(3, 'UTC')
)
ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (tenant_id, toStartOfHour(occurred_at), event_id)
TTL occurred_at + INTERVAL 13 MONTH
SETTINGS index_granularity = 8192;
