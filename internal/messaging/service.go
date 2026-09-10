package messaging

import (
	"context"

	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Service implements messaging use-cases on top of the interaction core.
type Service struct {
	interactions *interactions.Service
	provider     MessagingProvider
}

func NewService(interactions *interactions.Service, provider MessagingProvider) *Service {
	return &Service{interactions: interactions, provider: provider}
}

// SendInput is the validated send request.
type SendInput struct {
	CustomerID string   `json:"customer_id"`
	Channel    string   `json:"channel"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Body       string   `json:"body"`
	MediaURLs  []string `json:"media_urls"`
}

// Send creates an outbound messaging interaction and hands delivery to the
// provider. Delivery receipts arrive as signed webhooks (single-effect via
// idempotency keys).
func (s *Service) Send(ctx context.Context, tenantID string, in SendInput) (*interactions.Rec, error) {
	rec, err := s.interactions.Create(ctx, tenantID, interactions.CreateInput{
		CustomerID:  in.CustomerID,
		Channel:     interactions.Channel(in.Channel),
		Direction:   interactions.DirectionOutbound,
		Source:      in.From,
		Destination: in.To,
		Provider:    string(in.Channel),
	})
	if err != nil {
		return nil, err
	}
	msg := &Message{
		InteractionID: rec.ID,
		Channel:       Channel(in.Channel),
		From:          in.From,
		To:            in.To,
		Body:          in.Body,
		MediaURLs:     in.MediaURLs,
	}
	if err := ValidateMessage(msg); err != nil {
		_, _ = s.interactions.Transition(ctx, tenantID, rec.ID, interactions.StatusFailed, "validation_failed")
		return nil, err
	}
	msg.TenantID = tenantID

	// pending→active = "queued→sent" from the platform's perspective; the
	// provider's delivered/read receipts continue the lifecycle via webhooks.
	rec, err = s.interactions.Transition(ctx, tenantID, rec.ID, interactions.StatusActive, "")
	if err != nil {
		return nil, err
	}
	if err := s.provider.Send(ctx, *msg); err != nil {
		_, _ = s.interactions.Transition(ctx, tenantID, rec.ID, interactions.StatusFailed, "provider_send_failed")
		return nil, apperrors.Internal("message.provider_failed", "provider rejected the message").WithCause(err)
	}
	return s.interactions.Get(ctx, tenantID, rec.ID)
}
