-- Orvexa 0003 — Conversation & interaction domains.
-- A conversation is the continuous customer relationship; an interaction is
-- ONE communication inside it (a call, a WhatsApp message thread, an email).
-- This distinction is the platform's most important domain decision.

CREATE TABLE IF NOT EXISTS conversations (
    id               UUID PRIMARY KEY,
    tenant_id        UUID NOT NULL REFERENCES tenants(id),
    customer_id      UUID NOT NULL REFERENCES customers(id),
    channel          TEXT NOT NULL CHECK (channel IN
                     ('voice','whatsapp','sms','email','chat','ussd')),
    status           TEXT NOT NULL DEFAULT 'open'
                     CHECK (status IN ('open','closed')),
    subject          TEXT,
    assigned_agent_id UUID,
    assigned_queue_id UUID,
    unread_count     INT NOT NULL DEFAULT 0,
    last_interaction_at TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_conversations_tenant_status ON conversations(tenant_id, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_customer ON conversations(customer_id);
CREATE INDEX IF NOT EXISTS idx_conversations_agent ON conversations(assigned_agent_id) WHERE assigned_agent_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS conversation_participants (
    conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    participant_type TEXT NOT NULL CHECK (participant_type IN ('customer','agent','ai_agent','system')),
    participant_id  TEXT NOT NULL,
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    left_at         TIMESTAMPTZ,
    PRIMARY KEY (conversation_id, participant_type, participant_id)
);

-- The unified interaction abstraction — the common language of the platform.
CREATE TABLE IF NOT EXISTS interactions (
    id              UUID PRIMARY KEY,
    tenant_id       UUID NOT NULL REFERENCES tenants(id),
    conversation_id UUID NOT NULL REFERENCES conversations(id),
    customer_id     UUID NOT NULL REFERENCES customers(id),

    channel         TEXT NOT NULL CHECK (channel IN
                    ('voice','whatsapp','sms','email','chat','ussd')),
    direction       TEXT NOT NULL CHECK (direction IN ('inbound','outbound')),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','active','wrapup','completed','failed','canceled')),

    source          TEXT NOT NULL,   -- provider-neutral origin (E.164, email, handle)
    destination     TEXT NOT NULL,   -- provider-neutral target

    assigned_agent_id UUID,
    assigned_queue_id UUID,

    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    answered_at     TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    end_reason      TEXT,

    provider        TEXT NOT NULL DEFAULT 'internal',  -- which adapter owns the leg
    provider_ref    TEXT,                              -- provider-side call/message id
    idempotency_key TEXT,                              -- dedupe externally-triggered creates

    attributes      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, provider, provider_ref)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_interactions_idem
    ON interactions(tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_interactions_conversation ON interactions(conversation_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_interactions_tenant_status ON interactions(tenant_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_interactions_agent ON interactions(assigned_agent_id) WHERE assigned_agent_id IS NOT NULL;
