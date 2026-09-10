// Package telephony owns the voice interaction plane: the provider-agnostic
// VoiceProvider port and the call use-cases. The core never knows whether the
// carrier is Twilio, Africa's Talking, FreeSWITCH or the built-in simulator —
// it sees PlaceCall/Answer/Hangup/Transfer/Hold/Resume.
package telephony

import (
	"context"
)

// CallCommand is a provider-neutral call instruction for the provider leg.
type CallCommand struct {
	InteractionID string // platform interaction id (provider_ref linkage)
	From          string // E.164 or internal extension
	To            string // E.164 or internal extension
	// ProviderOptions carries adapter-specific tuning (callers, recording,
	// IVR entry) — validated by the adapter, opaque to the core.
	ProviderOptions map[string]any
}

// VoiceProvider is the hexagonal port for voice carriers.
type VoiceProvider interface {
	PlaceCall(ctx context.Context, cmd CallCommand) error
	Hangup(ctx context.Context, providerRef string) error
	Transfer(ctx context.Context, providerRef, destination string) error
	Hold(ctx context.Context, providerRef string) error
	Resume(ctx context.Context, providerRef string) error
}

// ProviderRef returns the provider-side identifier convention used by the
// platform: the interaction id doubles as the provider leg reference for
// adapters that support caller-supplied references; adapters that mint their
// own ids report them back through signed webhooks carrying the interaction id.
func ProviderRef(interactionID string) string { return interactionID }
