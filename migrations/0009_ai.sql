-- Orvexa 0009 — Intelligence plane: AI agent configuration, tool policies,
-- usage metering. AI config is data; execution goes through the AI gateway;
-- side-effects go through the tool gateway. AI never holds credentials.

CREATE TABLE IF NOT EXISTS ai_agents (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    name        TEXT NOT NULL,
    model_hint  TEXT NOT NULL DEFAULT 'rules-v1',  -- logical model; gateway maps to provider/model
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','paused','retired')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS ai_agent_versions (
    id          UUID PRIMARY KEY,
    agent_id    UUID NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,
    version     INT NOT NULL,
    system_prompt TEXT NOT NULL,
    config_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, version)
);

CREATE TABLE IF NOT EXISTS ai_agent_tools (
    agent_id    UUID NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,
    tool_name   TEXT NOT NULL,
    allowed     BOOLEAN NOT NULL DEFAULT true,
    max_calls_per_invocation INT NOT NULL DEFAULT 3,
    PRIMARY KEY (agent_id, tool_name)
);

CREATE TABLE IF NOT EXISTS ai_agent_policies (
    agent_id    UUID NOT NULL REFERENCES ai_agents(id) ON DELETE CASCADE,
    policy      TEXT NOT NULL,   -- e.g. 'require_human_confirmation', 'no_pii_in_output'
    config_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (agent_id, policy)
);

CREATE TABLE IF NOT EXISTS ai_usage (
    id            UUID PRIMARY KEY,
    tenant_id     UUID NOT NULL,
    agent_id      UUID,
    interaction_id UUID,
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    input_tokens  INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cost_microus  BIGINT NOT NULL DEFAULT 0,
    latency_ms    INT NOT NULL DEFAULT 0,
    outcome       TEXT NOT NULL CHECK (outcome IN ('completed','failed','timeout','refused')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ai_usage_tenant ON ai_usage(tenant_id, created_at DESC);
