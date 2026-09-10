-- Orvexa 0005 — Case domain.
-- Cases are independent from interactions: one case can span WhatsApp, calls,
-- emails and internal notes. Links associate interactions with a case.

CREATE TABLE IF NOT EXISTS cases (
    id          UUID PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    customer_id UUID NOT NULL REFERENCES customers(id),
    ref         TEXT NOT NULL,                   -- human reference CASE-2026-0001
    subject     TEXT NOT NULL,
    description TEXT,
    status      TEXT NOT NULL DEFAULT 'open'
                CHECK (status IN ('open','in_progress','pending_customer','resolved','closed','canceled')),
    priority    TEXT NOT NULL DEFAULT 'normal'
                CHECK (priority IN ('low','normal','high','urgent')),
    assigned_agent_id UUID,
    resolved_at TIMESTAMPTZ,
    closed_at   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, ref)
);
CREATE INDEX IF NOT EXISTS idx_cases_tenant_status ON cases(tenant_id, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_cases_customer ON cases(customer_id);

CREATE TABLE IF NOT EXISTS case_notes (
    id          UUID PRIMARY KEY,
    case_id     UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    tenant_id   UUID NOT NULL REFERENCES tenants(id),
    author_type TEXT NOT NULL CHECK (author_type IN ('agent','ai_agent','system')),
    author_id   TEXT NOT NULL,
    body        TEXT NOT NULL,
    internal    BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_case_notes_case ON case_notes(case_id, created_at);

CREATE TABLE IF NOT EXISTS case_interactions (
    case_id        UUID NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    interaction_id UUID NOT NULL REFERENCES interactions(id),
    linked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (case_id, interaction_id)
);
