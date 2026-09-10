package telephony

import (
	"context"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Service implements voice call use-cases on top of the interaction core.
type Service struct {
	interactions *interactions.Service
	provider     VoiceProvider
}

func NewService(interactions *interactions.Service, provider VoiceProvider) *Service {
	return &Service{interactions: interactions, provider: provider}
}

// PlaceCallInput is the validated place-call request.
type PlaceCallInput struct {
	CustomerID string         `json:"customer_id"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	AgentID    string         `json:"agent_id"`
	QueueID    string         `json:"queue_id"`
	Options    map[string]any `json:"options"`
}

// PlaceCall creates a pending outbound voice interaction and hands the leg to
// the provider. The provider's lifecycle webhooks drive pending→active→…
// through the webhook gateway — the API returns immediately (critical path
// principle: nothing beyond telephony blocks the call).
func (s *Service) PlaceCall(ctx context.Context, tenantID string, in PlaceCallInput) (*interactions.Rec, error) {
	if in.CustomerID == "" {
		return nil, apperrors.Invalid("call.customer_required", "customer_id is required")
	}
	if in.To == "" {
		return nil, apperrors.Invalid("call.to_required", "to (E.164 or extension) is required")
	}
	from := in.From
	if from == "" {
		from = "orvexa-voice" // platform default caller id; adapters may override
	}

	rec, err := s.interactions.Create(ctx, tenantID, interactions.CreateInput{
		CustomerID:  in.CustomerID,
		Channel:     interactions.ChannelVoice,
		Direction:   interactions.DirectionOutbound,
		Source:      from,
		Destination: in.To,
		Provider:    "voice",
		Attributes: map[string]any{
			"options": in.Options,
		},
	})
	if err != nil {
		return nil, err
	}
	if in.AgentID != "" || in.QueueID != "" {
		if rec, err = s.interactions.Assign(ctx, tenantID, rec.ID, in.AgentID, in.QueueID); err != nil {
			return nil, err
		}
	}
	opts := in.Options
	if opts == nil {
		opts = map[string]any{}
	}
	opts["tenant_id"] = tenantID // adapters map tenant → provider account/config
	if err := s.provider.PlaceCall(ctx, CallCommand{
		InteractionID:   rec.ID,
		From:            from,
		To:              in.To,
		ProviderOptions: opts,
	}); err != nil {
		// leg failed to start: fail the interaction (terminal, audited)
		_, _ = s.interactions.Transition(ctx, tenantID, rec.ID, interactions.StatusFailed, "provider_place_failed")
		return nil, apperrors.Internal("call.provider_failed", "provider rejected the call").WithCause(err)
	}
	return s.interactions.Get(ctx, tenantID, rec.ID)
}

// ApplyAction runs an operator/provider action against a live call leg.
func (s *Service) ApplyAction(ctx context.Context, tenantID, interactionID, action, destination string) (*interactions.Rec, error) {
	rec, err := s.interactions.Get(ctx, tenantID, interactionID)
	if err != nil {
		return nil, err
	}
	switch action {
	case "answer":
		// operator bridged the call platform-side; the provider's ringing
		// webhooks may already have marked it active — same-state is a no-op
		// upstream, so this is safe to apply unconditionally.
		if rec.Status == interactions.StatusPending {
			rec, err = s.interactions.Transition(ctx, tenantID, rec.ID, interactions.StatusActive, "")
		}
	case "hangup":
		// the provider's call.ended webhook drives pending/active→wrapup;
		// the service never duplicates provider-driven transitions.
		if err := s.provider.Hangup(ctx, ProviderRef(rec.ID)); err != nil {
			return nil, apperrors.Internal("call.provider_failed", "provider hangup failed").WithCause(err)
		}
	case "transfer":
		if destination == "" {
			return nil, apperrors.Invalid("call.destination_required", "transfer requires destination")
		}
		if err := s.provider.Transfer(ctx, ProviderRef(rec.ID), destination); err != nil {
			return nil, apperrors.Internal("call.provider_failed", "provider transfer failed").WithCause(err)
		}
	case "hold":
		err = s.provider.Hold(ctx, ProviderRef(rec.ID))
	case "resume":
		err = s.provider.Resume(ctx, ProviderRef(rec.ID))
	case "complete":
		// agent finished after-call work: wrapup→completed (or active→completed)
		rec, err = s.interactions.Transition(ctx, tenantID, rec.ID, interactions.StatusCompleted, "complete")
	default:
		return nil, apperrors.Invalid("call.unknown_action",
			"action must be answer|hangup|transfer|hold|resume|complete")
	}
	if err != nil {
		return nil, err
	}
	return s.interactions.Get(ctx, tenantID, rec.ID)
}
