// Package webhooks is the hardened entry point for external providers.
//
// Contract (architecture doc §27): authentication → signature validation →
// deduplication → rate limiting → persistence. Business logic never runs in
// the HTTP handler: accepted events are persisted to the provider_events
// ledger and the communications domain consumes them.
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"

	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/idempotency"
)

// MaxPayloadBytes caps webhook bodies (provider payloads are small).
const MaxPayloadBytes = 256 * 1024

// SignatureHeader carries the HMAC-SHA256 hex digest of the raw body.
const SignatureHeader = "X-Orvexa-Signature"

// Gateway persists validated provider webhooks with single-effect semantics.
type Gateway struct {
	pool   *pgxpool.Pool
	secret string
}

// NewGateway builds the gateway. secret is the platform-wide HMAC secret
// (ORVEXA_WEBHOOK_HMAC_SECRET); tenant-specific secret resolution is an
// adapter concern for the real carrier integrations.
func NewGateway(pool *pgxpool.Pool, secret string) *Gateway {
	return &Gateway{pool: pool, secret: secret}
}

// ComputeSignature is the HMAC-SHA256 hex digest providers (and the built-in
// simulator) must deliver in X-Orvexa-Signature over the raw body.
func ComputeSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateSignature is timing-safe. An empty secret always rejects: the
// platform refuses unsigned ingestion rather than opening an unauthenticated
// write path.
func ValidateSignature(secret string, body []byte, provided string) bool {
	if secret == "" {
		return false
	}
	expected := ComputeSignature(secret, body)
	return hmac.Equal([]byte(expected), []byte(provided))
}

// IngestResult reports what happened to a webhook submission.
type IngestResult struct {
	EventID   string `json:"event_id"`
	Duplicate bool   `json:"duplicate"`
	Accepted  bool   `json:"accepted"`
}

// Ingest validates the signature, derives the idempotency key from the raw
// payload and persists the event. Duplicate deliveries return the original
// event id with duplicate=true and are NOT an error — replay safety is the
// whole point.
func (g *Gateway) Ingest(ctx context.Context, provider string, body []byte, signature string) (*IngestResult, error) {
	if provider == "" || len(provider) > 64 {
		return nil, apperrors.Invalid("webhook.invalid_provider", "provider path is required (1-64 chars)")
	}
	if len(body) == 0 {
		return nil, apperrors.Invalid("webhook.empty_body", "request body is required")
	}
	if len(body) > MaxPayloadBytes {
		return nil, apperrors.Invalid("webhook.body_too_large", "payload exceeds 256 KiB")
	}
	if !ValidateSignature(g.secret, body, signature) {
		return nil, apperrors.Unauth("webhook.invalid_signature", "signature validation failed")
	}

	// Stable provider event id derived from the payload: replays of the SAME
	// event dedupe even when the provider sends no explicit id. Distinct
	// events with byte-identical payloads are also deduped — acceptable at
	// this layer because the communications consumer carries the provider
	// interaction id in the payload for true identity.
	eventID := idempotency.Derive("webhook", provider, string(body))

	res, err := g.pool.Exec(ctx, `
		INSERT INTO provider_events (id, provider, provider_event_id, payload, signature_valid)
		VALUES ($1,$2,$3,$4,true)
		ON CONFLICT (provider, provider_event_id) DO NOTHING`,
		uuid.NewString(), provider, eventID, body)
	if err != nil {
		return nil, apperrors.Internal("webhook.persist_failed", "ingestion failed").WithCause(err)
	}
	if res.RowsAffected() == 0 {
		var existing string
		if err := g.pool.QueryRow(ctx, `SELECT id FROM provider_events
			WHERE provider = $1 AND provider_event_id = $2`, provider, eventID).Scan(&existing); err != nil {
			return nil, apperrors.Internal("webhook.lookup_failed", "ingestion failed").WithCause(err)
		}
		return &IngestResult{EventID: existing, Duplicate: true, Accepted: true}, nil
	}
	return &IngestResult{EventID: eventID, Duplicate: false, Accepted: true}, nil
}

// MarkProcessed records consumption completion (best-effort).
func (g *Gateway) MarkProcessed(ctx context.Context, eventID string) {
	_, _ = g.pool.Exec(ctx, `UPDATE provider_events SET processed_at = now()
		WHERE provider_event_id = $1 AND processed_at IS NULL`, eventID)
}
