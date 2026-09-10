-- Orvexa 0002 — Customer domain: identity with multiple identifiers.
-- One customer, many channels: phone / whatsapp / email / external CRM ids
-- all resolve to the same customer within a tenant.

CREATE TABLE IF NOT EXISTS customers (
    id           UUID PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    display_name TEXT,
    company      TEXT,
    language     TEXT NOT NULL DEFAULT 'en',
    timezone     TEXT NOT NULL DEFAULT 'Africa/Nairobi',
    attributes   JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_customers_tenant ON customers(tenant_id, created_at DESC);

-- The resolution backbone: (tenant, type, value) is unique so a channel
-- identifier can never fork into two customers. Values are stored normalized
-- (phone → E.164 digits, email → lowercase) by the service layer.
CREATE TABLE IF NOT EXISTS customer_identifiers (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    type        TEXT NOT NULL CHECK (type IN ('phone','whatsapp','email','external_crm','handle')),
    value       TEXT NOT NULL,
    is_primary  BOOLEAN NOT NULL DEFAULT false,
    verified_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, type, value)
);
CREATE INDEX IF NOT EXISTS idx_identifiers_customer ON customer_identifiers(customer_id);

CREATE TABLE IF NOT EXISTS customer_tags (
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    tag         TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, customer_id, tag)
);
