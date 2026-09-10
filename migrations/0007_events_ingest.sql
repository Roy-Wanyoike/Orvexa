-- Orvexa 0007 — Inbound webhook ingestion: provider event ledger.
-- Every accepted inbound provider webhook persists here FIRST (dedup by
-- (provider, provider_event_id) unique); domain consumption reads from this
-- ledger. This is what makes provider replays single-effect.

CREATE TABLE IF NOT EXISTS provider_events (
    id                UUID PRIMARY KEY,
    provider          TEXT NOT NULL,                 -- twilio|whatsapp_cloud|africastalking|simulator|...
    provider_event_id TEXT NOT NULL,                 -- provider-side unique id (or derived idempotency key)
    tenant_hint       UUID,                          -- resolved later when the credential maps to a tenant
    payload           JSONB NOT NULL,
    signature_valid   BOOLEAN NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    UNIQUE (provider, provider_event_id)
);
CREATE INDEX IF NOT EXISTS idx_provider_events_unprocessed ON provider_events(received_at) WHERE processed_at IS NULL;
