// Package comms hosts the communications-plane glue: the built-in simulator
// provider (a faithful local stand-in for real carriers) and the processor
// that turns signed provider webhooks into interaction lifecycle transitions.
package comms

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// IngestFunc is the port the simulator uses to deliver signed provider
// webhooks. In production wiring it performs HTTP POST against the platform's
// own webhook endpoint; tests/inproc wiring calls the gateway directly. Both
// paths pass the exact same HMAC-validated entry — no provider bypasses the
// signature check.
type IngestFunc func(ctx context.Context, provider string, body []byte, signature string) error

// ProviderEvent is the provider→platform webhook payload contract. Real
// carrier adapters translate their native payloads into this shape.
type ProviderEvent struct {
	Event         string `json:"event"`            // call.ringing|call.connected|call.completed|call.failed|message.delivered|message.read
	InteractionID string `json:"interaction_id"`   // platform interaction id
	TenantID      string `json:"tenant_id"`
	Timestamp     string `json:"timestamp"`        // RFC3339, provider-side occurrence
	Detail        string `json:"detail,omitempty"` // e.g. hangup cause
}

// InteractionCore is the slice of the interaction service the processor needs.
type InteractionCore interface {
	Get(ctx context.Context, tenantID, id string) (*interactions.Rec, error)
	Transition(ctx context.Context, tenantID, id string, to interactions.Status, endReason string) (*interactions.Rec, error)
}

// Processor maps validated provider events to guarded transitions. Duplicate
// provider events are deduped at the webhook gateway; a transition that cannot
// legally apply returns 409 — surfaced, never silently swallowed.
type Processor struct {
	Interactions InteractionCore
}

// Process applies one provider event.
func (p *Processor) Process(ctx context.Context, ev *ProviderEvent) error {
	if ev == nil || ev.InteractionID == "" || ev.TenantID == "" {
		return apperrors.Invalid("comms.malformed_event", "event, interaction_id and tenant_id are required")
	}
	if _, err := uuid.Parse(ev.InteractionID); err != nil {
		return apperrors.Invalid("comms.malformed_event", "interaction_id must be a platform interaction id")
	}
	var to interactions.Status
	switch ev.Event {
	case "call.ringing", "call.connected", "message.sent", "message.delivered":
		to = interactions.StatusActive
	case "call.ended", "call.wrapup", "message.wrapup":
		to = interactions.StatusWrapup // customer left; after-call work begins
	case "message.read":
		to = interactions.StatusCompleted
	case "call.failed", "message.failed":
		to = interactions.StatusFailed
	default:
		return apperrors.Invalid("comms.unknown_event", "unsupported provider event "+ev.Event)
	}

	// Carriers resend lifecycle events routinely: same-state delivery is a
	// no-op, not an error (idempotent processing contract).
	cur, err := p.Interactions.Get(ctx, ev.TenantID, ev.InteractionID)
	if err != nil {
		return err
	}
	if cur.Status == to {
		return nil
	}
	_, err = p.Interactions.Transition(ctx, ev.TenantID, ev.InteractionID, to, ev.Detail)
	return err
}

// Encode builds the wire body for an event (used by the simulator + adapters).
func (ev *ProviderEvent) Encode() ([]byte, error) {
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return json.Marshal(ev)
}
