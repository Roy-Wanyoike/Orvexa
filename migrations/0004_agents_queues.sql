-- Orvexa 0004 — Agent identity & queues.
-- Agent identity (who they are, what they can do) is separate from agent
-- presence (where they are now) — presence lands with routing (0007).

CREATE TABLE IF NOT EXISTS agents (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    external_identity TEXT NOT NULL,           -- IdP subject / username
    display_name TEXT NOT NULL,
    email       TEXT,
    language    TEXT NOT NULL DEFAULT 'en',
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active','suspended','offboarded')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, external_identity)
);

CREATE TABLE IF NOT EXISTS agent_skills (
    agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill    TEXT NOT NULL,                      -- e.g. 'billing', 'swahili', 'escalations'
    level    INT NOT NULL DEFAULT 1 CHECK (level BETWEEN 1 AND 5),
    PRIMARY KEY (agent_id, skill)
);
CREATE INDEX IF NOT EXISTS idx_agent_skills_skill ON agent_skills(skill);

CREATE TABLE IF NOT EXISTS queues (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    description TEXT,
    priority    INT NOT NULL DEFAULT 5 CHECK (priority BETWEEN 1 AND 10),
    business_hours_json JSONB,                   -- nullable = 24/7
    sla_seconds INT NOT NULL DEFAULT 60,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','closed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS queue_skills (
    queue_id UUID NOT NULL REFERENCES queues(id) ON DELETE CASCADE,
    skill    TEXT NOT NULL,
    PRIMARY KEY (queue_id, skill)
);
