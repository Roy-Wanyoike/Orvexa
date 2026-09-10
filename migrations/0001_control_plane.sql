-- Orvexa 0001 — Control plane: organizations, tenants, api_keys, audit_events.
-- Forward-only. Every subsequent migration builds on this baseline.

CREATE TABLE IF NOT EXISTS organizations (
    id          UUID PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A tenant is an isolated operating boundary inside an organization
-- (e.g. a business unit, brand or region). All domain rows carry tenant_id.
CREATE TABLE IF NOT EXISTS tenants (
    id               UUID PRIMARY KEY,
    organization_id  UUID NOT NULL REFERENCES organizations(id),
    name             TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','suspended','closed')),
    config_json      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tenants_org ON tenants(organization_id);

-- API keys authenticate machine + human principals. Only the SHA-256 hash of
-- the key is stored; the raw key is shown once at creation.
CREATE TABLE IF NOT EXISTS api_keys (
    id           UUID PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,           -- hex(sha256(raw_key))
    scopes       TEXT[] NOT NULL DEFAULT '{}',   -- e.g. {interactions:write,ai:invoke}
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','revoked')),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id);

-- Append-only audit trail for consequential actions across all planes.
-- Rows are never updated or deleted (enforced by convention + missing
-- UPDATE/DELETE grants in hardened deployments). Correlation id ties rows to
-- the request/event chain that caused them.
CREATE TABLE IF NOT EXISTS audit_events (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      UUID,
    actor_type     TEXT NOT NULL,                 -- user|agent|ai|system|integration
    actor_id       TEXT NOT NULL,
    action         TEXT NOT NULL,                 -- e.g. interaction.assigned
    resource_type  TEXT NOT NULL,
    resource_id    TEXT NOT NULL,
    before_json    JSONB,
    after_json     JSONB,
    reason         TEXT,
    ip_address     TEXT,
    correlation_id TEXT,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_tenant_time ON audit_events(tenant_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_resource ON audit_events(resource_type, resource_id);
