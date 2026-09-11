// Package interactions owns the unified interaction abstraction — the common
// language of the platform across voice, whatsapp, sms, email, chat and ussd.
// The state machine is pure so every transition rule is unit-testable.
package interactions

import (
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Status is the channel-independent interaction lifecycle state.
type Status string

const (
	StatusPending   Status = "pending"   // created, not yet flowing
	StatusActive    Status = "active"    // ringing / in flight / delivering
	StatusWrapup    Status = "wrapup"    // work continuing after customer left
	StatusCompleted Status = "completed" // terminal success
	StatusFailed    Status = "failed"    // terminal failure
	StatusCanceled  Status = "canceled"  // terminal, abandoned before start
)

// Channel is the platform's channel vocabulary.
type Channel string

const (
	ChannelVoice    Channel = "voice"
	ChannelWhatsApp Channel = "whatsapp"
	ChannelSMS      Channel = "sms"
	ChannelEmail    Channel = "email"
	ChannelChat     Channel = "chat"
	ChannelUSSD     Channel = "ussd"
)

// Direction of the communication.
type Direction string

const (
	DirectionInbound  Direction = "inbound"
	DirectionOutbound Direction = "outbound"
)

// transitions encodes the lifecycle. Terminal states map to empty slices.
var transitions = map[Status][]Status{
	StatusPending:   {StatusActive, StatusCanceled, StatusFailed},
	StatusActive:    {StatusWrapup, StatusCompleted, StatusFailed},
	StatusWrapup:    {StatusCompleted, StatusFailed},
	StatusCompleted: {},
	StatusFailed:    {},
	StatusCanceled:  {},
}

// CanTransition reports whether from→to is legal.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// NextStates lists legal successors (used in 409 error payloads + docs).
func NextStates(from Status) []Status {
	out := make([]Status, 0, len(transitions[from]))
	out = append(out, transitions[from]...)
	return out
}

// Transition applies the lifecycle rule to a record.
func Transition(from, to Status) error {
	if from == to {
		return apperrors.Conflict("interaction.invalid_transition",
			"interaction is already "+string(from))
	}
	if !CanTransition(from, to) {
		return apperrors.Conflict("interaction.invalid_transition",
			"cannot move interaction from "+string(from)+" to "+string(to)).
			WithDetails(map[string]any{"from": from, "requested": to, "allowed": NextStates(from)})
	}
	return nil
}

// Rec is the interaction record as services see it. It doubles as the wire
// model of every interaction-plane response (POST /interactions, /calls,
// /messages; GET /interactions/{id}), so these json tags ARE the published
// contract (#101): snake_case keys exactly as api/openapi/orvexa-v1.yaml
// documents them — Go field names never reach the wire.
type Rec struct {
	ID              string         `json:"id"`
	TenantID        string         `json:"tenant_id"`
	ConversationID  string         `json:"conversation_id"`
	CustomerID      string         `json:"customer_id"`
	Channel         Channel        `json:"channel"`
	Direction       Direction      `json:"direction"`
	Status          Status         `json:"status"`
	Source          string         `json:"source"`
	Destination     string         `json:"destination"`
	AssignedAgentID string         `json:"assigned_agent_id,omitempty"`
	AssignedQueueID string         `json:"assigned_queue_id,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	AnsweredAt      *time.Time     `json:"answered_at,omitempty"`
	EndedAt         *time.Time     `json:"ended_at,omitempty"`
	EndReason       string         `json:"end_reason,omitempty"`
	Provider        string         `json:"provider"`
	ProviderRef     string         `json:"provider_ref,omitempty"`
	IdempotencyKey  string         `json:"idempotency_key,omitempty"`
	Attributes      map[string]any `json:"attributes"`
}

// Terminal reports whether the status ends the interaction.
func (s Status) Terminal() bool {
	return len(transitions[s]) == 0
}
