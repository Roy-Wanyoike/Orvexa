-- Orvexa 0010 — Execution plane: durable workflows + analytics facts.
-- Workflows: crash-safe state in DB; the executor advances steps and
-- schedules timers. Temporal adapter path documented in ADR-0006.

CREATE TABLE IF NOT EXISTS workflow_instances (
    id           UUID PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    type         TEXT NOT NULL CHECK (type IN ('callback','collections')),
    status       TEXT NOT NULL DEFAULT 'running'
                 CHECK (status IN ('running','waiting_timer','completed','failed','canceled')),
    payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    state_json   JSONB NOT NULL DEFAULT '{}'::jsonb,
    current_step TEXT NOT NULL DEFAULT '',
    next_run_at  TIMESTAMPTZ,
    attempts     INT NOT NULL DEFAULT 0,
    error        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workflow_due ON workflow_instances(status, next_run_at) WHERE status IN ('running','waiting_timer');

CREATE TABLE IF NOT EXISTS workflow_steps (
    id           BIGSERIAL PRIMARY KEY,
    instance_id  UUID NOT NULL REFERENCES workflow_instances(id) ON DELETE CASCADE,
    step         TEXT NOT NULL,
    status       TEXT NOT NULL CHECK (status IN ('completed','failed','skipped')),
    detail_json  JSONB NOT NULL DEFAULT '{}'::jsonb,
    executed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_workflow_steps_instance ON workflow_steps(instance_id, executed_at);

-- Analytics facts: appended by the analytics consumer from the bus.
-- Read APIs aggregate over bounded windows; OLTP never hosts dashboards.
CREATE TABLE IF NOT EXISTS interaction_fact (
    event_id      UUID PRIMARY KEY,              -- envelope id (consumer dedupe)
    tenant_id     UUID NOT NULL,
    interaction_id UUID NOT NULL,
    channel       TEXT NOT NULL,
    direction     TEXT NOT NULL,
    status        TEXT NOT NULL,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_interaction_fact_window ON interaction_fact(tenant_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS usage_fact (
    event_id   UUID PRIMARY KEY,
    tenant_id  UUID NOT NULL,
    metric     TEXT NOT NULL,
    amount     BIGINT NOT NULL DEFAULT 0,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_usage_fact_window ON usage_fact(tenant_id, occurred_at DESC);
