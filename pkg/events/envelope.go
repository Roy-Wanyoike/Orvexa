// Package events defines Orvexa's canonical event envelope and topic registry.
//
// Every domain fact crossing process boundaries (outbox → bus → consumers)
// travels as an Envelope. Topics are hierarchical "domain.entity.verb" strings
// from a fixed registry — ad-hoc topic strings are rejected so the event
// catalog stays auditable.
package events

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Envelope is the canonical wire format for all Orvexa events.
type Envelope struct {
	ID            string         `json:"id"`                       // unique event id (uuid)
	Type          string         `json:"type"`                     // topic from the registry, e.g. interaction.created
	Source        string         `json:"source"`                   // emitting service, e.g. "orvexa-api"
	Subject       string         `json:"subject,omitempty"`        // primary resource id, e.g. interaction id
	TenantID      string         `json:"tenant_id"`                // tenant the event belongs to
	CorrelationID string         `json:"correlation_id,omitempty"` // request-scoped causal id
	Time          time.Time      `json:"time"`                     // occurrence time (UTC)
	Data          map[string]any `json:"data"`                     // payload (topic-specific)
}

// Validate enforces envelope invariants.
func (e *Envelope) Validate() error {
	if e == nil {
		return fmt.Errorf("envelope is nil")
	}
	if _, err := uuid.Parse(e.ID); err != nil {
		return fmt.Errorf("id must be a uuid")
	}
	if !IsRegistered(e.Type) {
		return fmt.Errorf("unregistered topic %q", e.Type)
	}
	if e.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	return nil
}

// New builds and validates an envelope, returning an error on invariant breach.
func New(topic, source, subject, tenantID, correlationID string, data map[string]any) (*Envelope, error) {
	e := &Envelope{
		ID:            uuid.NewString(),
		Type:          topic,
		Source:        source,
		Subject:       subject,
		TenantID:      tenantID,
		CorrelationID: correlationID,
		Time:          time.Now().UTC(),
		Data:          data,
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return e, nil
}
