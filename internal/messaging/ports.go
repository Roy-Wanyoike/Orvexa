// Package messaging owns the messaging plane: the provider-agnostic
// MessagingProvider port (WhatsApp / SMS / email adapters implement it) and
// the message use-cases. Sending a message creates an outbound interaction in
// the customer's open conversation — one continuous context.
package messaging

import (
	"context"
	"strings"
	"unicode/utf8"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Channel selects the messaging medium for the provider adapter.
type Channel string

const (
	ChannelWhatsApp Channel = "whatsapp"
	ChannelSMS      Channel = "sms"
	ChannelEmail    Channel = "email"
)

// Message is the provider-neutral outbound message.
type Message struct {
	InteractionID string
	TenantID      string // tenant the message belongs to (adapters map it to provider accounts)
	Channel       Channel
	From          string
	To            string
	Body          string
	// MediaURLs attach attachments (WhatsApp media, email attachments);
	// adapters validate scheme and reachability.
	MediaURLs []string
}

// MessagingProvider is the hexagonal port for messaging carriers.
type MessagingProvider interface {
	Send(ctx context.Context, msg Message) error
}

// ValidateMessage enforces the core-side invariants adapters may rely on.
func ValidateMessage(msg *Message) error {
	if msg.InteractionID == "" {
		return apperrors.Invalid("message.interaction_required", "interaction id is required")
	}
	switch msg.Channel {
	case ChannelWhatsApp, ChannelSMS, ChannelEmail:
	default:
		return apperrors.Invalid("message.invalid_channel", "channel must be whatsapp|sms|email")
	}
	if strings.TrimSpace(msg.To) == "" {
		return apperrors.Invalid("message.to_required", "destination is required")
	}
	if strings.TrimSpace(msg.Body) == "" && len(msg.MediaURLs) == 0 {
		return apperrors.Invalid("message.body_required", "body or media is required")
	}
	if utf8.RuneCountInString(msg.Body) > 4096 {
		return apperrors.Invalid("message.body_too_long", "body must be at most 4096 chars")
	}
	return nil
}
