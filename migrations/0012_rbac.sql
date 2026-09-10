-- Orvexa 0012 — Capability RBAC: role bindings + capability catalog ([O-29],
-- issue #38). Division of trust: the IdP owns IDENTITY (sub/iss/aud/exp via
-- OIDC); Orvexa owns AUTHORIZATION. A token asserts who the caller is; the
-- rows here decide what they may do. role_bindings.principal is the IdP
-- subject (the token `sub` claim), scoped per tenant — never an email or
-- mutable username. The embedded catalog in internal/identity/roles.go and
-- the matrix in docs/rbac.md MUST stay in sync with the seeds below
-- (asserted by the identity package tests).

-- One row per (tenant, IdP subject, role). Binding administration is a
-- control-plane concern (no runtime write API ships with the identity
-- plane); revoking (status='revoked') drops capabilities immediately even
-- while the caller's token is still cryptographically valid, subject to the
-- resolver's short TTL cache.
CREATE TABLE IF NOT EXISTS role_bindings (
    id         UUID PRIMARY KEY,
    tenant_id  UUID NOT NULL REFERENCES tenants(id),
    principal  TEXT NOT NULL,                  -- IdP subject (`sub` claim)
    role       TEXT NOT NULL
               CHECK (role IN ('viewer','agent','supervisor','admin')),
    status     TEXT NOT NULL DEFAULT 'active'
               CHECK (status IN ('active','revoked')),
    granted_by TEXT,                           -- actor that granted the binding
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, principal, role)
);
CREATE INDEX IF NOT EXISTS idx_role_bindings_lookup ON role_bindings(tenant_id, principal, status);

-- Closed capability catalog: role → capability set. The runtime enforcement
-- point is identity.RequireCapability; capability names are stable machine
-- contracts (docs/rbac.md §capability catalog).
CREATE TABLE IF NOT EXISTS capability_catalog (
    role       TEXT NOT NULL
               CHECK (role IN ('viewer','agent','supervisor','admin')),
    capability TEXT NOT NULL
               CHECK (capability IN ('interaction.read','interaction.write',
                                     'case.read','case.write',
                                     'workflow.run','search.read','admin.org')),
    PRIMARY KEY (role, capability)
);

-- Seeds mirror internal/identity/roles.go roleCapabilities exactly
-- (escalating privilege: viewer ⊂ agent ⊂ supervisor ⊂ admin). Idempotent.
INSERT INTO capability_catalog (role, capability) VALUES
    ('viewer',     'interaction.read'),
    ('viewer',     'case.read'),
    ('viewer',     'search.read'),
    ('agent',      'interaction.read'),
    ('agent',      'interaction.write'),
    ('agent',      'case.read'),
    ('agent',      'case.write'),
    ('agent',      'search.read'),
    ('supervisor', 'interaction.read'),
    ('supervisor', 'interaction.write'),
    ('supervisor', 'case.read'),
    ('supervisor', 'case.write'),
    ('supervisor', 'workflow.run'),
    ('supervisor', 'search.read'),
    ('admin',      'interaction.read'),
    ('admin',      'interaction.write'),
    ('admin',      'case.read'),
    ('admin',      'case.write'),
    ('admin',      'workflow.run'),
    ('admin',      'search.read'),
    ('admin',      'admin.org')
ON CONFLICT (role, capability) DO NOTHING;
