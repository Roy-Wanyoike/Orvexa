-- Orvexa 0008 — Routing decisions (immutable records) + agent presence.
-- Every routing decision is recorded: inputs, ranked candidates, outcome.

CREATE TABLE IF NOT EXISTS routing_decisions (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    interaction_id  UUID NOT NULL REFERENCES interactions(id),
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    inputs_json     JSONB NOT NULL,              -- skills, language, priority, hours context
    candidates_json JSONB NOT NULL,              -- ranked agents with scores
    outcome         TEXT NOT NULL CHECK (outcome IN ('assigned_agent','queued','no_agent_available')),
    assigned_agent_id UUID,
    assigned_queue_id UUID,
    latency_micros  INT NOT NULL DEFAULT 0,
    correlation_id  TEXT
);
CREATE INDEX IF NOT EXISTS idx_routing_decisions_tenant ON routing_decisions(tenant_id, decided_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_decisions_interaction ON routing_decisions(interaction_id);

-- Presence: fast-changing agent state. The DB row is the source of truth
-- (reconstructable); the in-process store is the hot cache.
CREATE TABLE IF NOT EXISTS agent_presence (
    agent_id    UUID PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    status      TEXT NOT NULL CHECK (status IN ('available','busy','wrapup','offline')),
    current_interaction_id UUID,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_presence_tenant ON agent_presence(tenant_id, status);
