-- Orvexa 0006 — Event infrastructure: transactional outbox + webhook delivery.
-- outbox_events is written in the SAME transaction as domain state; the
-- dispatcher publishes with at-least-once semantics; consumers dedupe by id.

CREATE TABLE IF NOT EXISTS outbox_events (
    id              UUID PRIMARY KEY,             -- = event envelope id (dedupe key)
    topic           TEXT NOT NULL,
    source          TEXT NOT NULL,
    subject         TEXT,
    tenant_id       UUID NOT NULL,
    correlation_id  TEXT,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','publishing','published','failed')),
    attempts        INT NOT NULL DEFAULT 0,
    lease_token     TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_outbox_dispatch ON outbox_events(status, occurred_at) WHERE status <> 'published';

-- Webhook delivery to EXTERNAL subscribers (outbound), retry ladder included.
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    url         TEXT NOT NULL,
    secret_ref  TEXT NOT NULL DEFAULT '',        -- where the signing secret lives (env/vault ref, never raw)
    events      TEXT[] NOT NULL DEFAULT '{}',    -- empty = all topics
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, url)
);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id            UUID PRIMARY KEY,
    endpoint_id   UUID NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
    event_id      UUID NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','delivered','failed','dead')),
    attempts      INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_status_code INT,
    last_error    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (endpoint_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at) WHERE status = 'pending';
