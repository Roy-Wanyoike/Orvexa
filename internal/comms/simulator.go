package comms

import (
	"context"
	"sync"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
)

// Simulator is the built-in carrier: a deterministic, stateful stand-in for
// real telephony/messaging providers. It implements the telephony and
// messaging ports and reports lifecycle progress exactly like a real carrier:
// by delivering signed provider webhooks through the IngestFunc port.
//
// The simulator is labelled provider=simulator in every delivery and performs
// no network I/O. Production carriers (twilio, whatsapp_cloud,
// africastalking) are separate adapters behind the same ports (roadmap O-10).
type Simulator struct {
	ingest IngestFunc
	signer Signer
	// Pace controls lifecycle progression speed. Zero = synchronous
	// progression, which keeps tests deterministic; production-like pacing
	// is config-driven.
	Pace time.Duration

	mu   sync.Mutex
	legs map[string]*simLeg
}

// Signer computes the HMAC signature over a webhook body.
type Signer func(body []byte) string

type simLeg struct {
	interactionID string
	tenantID      string
	held          bool
	transferred   string
}

// NewSimulator builds the simulator around signed-webhook delivery.
func NewSimulator(ingest IngestFunc, signer Signer, pace time.Duration) *Simulator {
	if pace < 0 {
		pace = 0
	}
	return &Simulator{ingest: ingest, signer: signer, Pace: pace, legs: map[string]*simLeg{}}
}

// PlaceCall implements telephony.VoiceProvider. It registers the leg and
// drives ringing → connected via signed webhooks (ended arrives on Hangup).
func (s *Simulator) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	tenantID, _ := cmd.ProviderOptions["tenant_id"].(string)
	if tenantID == "" {
		return errTenantRequired
	}
	s.mu.Lock()
	s.legs[cmd.InteractionID] = &simLeg{interactionID: cmd.InteractionID, tenantID: tenantID}
	s.mu.Unlock()

	s.emit(ctx, &ProviderEvent{Event: "call.ringing", InteractionID: cmd.InteractionID, TenantID: tenantID})
	s.emit(ctx, &ProviderEvent{Event: "call.connected", InteractionID: cmd.InteractionID, TenantID: tenantID})
	return nil
}

// Hangup implements telephony.VoiceProvider: the customer side ends —
// call.ended moves the interaction to wrapup through the processor.
func (s *Simulator) Hangup(ctx context.Context, providerRef string) error {
	leg, ok := s.takeLeg(providerRef)
	if !ok {
		return errUnknownLeg
	}
	s.emit(ctx, &ProviderEvent{
		Event: "call.ended", InteractionID: providerRef, TenantID: leg.tenantID, Detail: "hangup",
	})
	return nil
}

// Transfer implements telephony.VoiceProvider: the destination rings —
// faithful carrier behavior (a new ringing phase on the same leg).
func (s *Simulator) Transfer(ctx context.Context, providerRef, destination string) error {
	s.mu.Lock()
	leg, ok := s.legs[providerRef]
	if !ok {
		s.mu.Unlock()
		return errUnknownLeg
	}
	leg.transferred = destination
	tenant := leg.tenantID
	s.mu.Unlock()

	s.emit(ctx, &ProviderEvent{
		Event: "call.ringing", InteractionID: providerRef, TenantID: tenant, Detail: "transfer:" + destination,
	})
	return nil
}

// Hold implements telephony.VoiceProvider (leg state flip only).
func (s *Simulator) Hold(ctx context.Context, providerRef string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	leg, ok := s.legs[providerRef]
	if !ok {
		return errUnknownLeg
	}
	leg.held = true
	return nil
}

// Resume implements telephony.VoiceProvider.
func (s *Simulator) Resume(ctx context.Context, providerRef string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	leg, ok := s.legs[providerRef]
	if !ok {
		return errUnknownLeg
	}
	leg.held = false
	return nil
}

// Send implements messaging.MessagingProvider: sent → delivered receipts.
// The customer-side read receipt arrives later as message.read webhook.
func (s *Simulator) Send(ctx context.Context, msg messaging.Message) error {
	if msg.TenantID == "" {
		return errTenantRequired
	}
	s.mu.Lock()
	s.legs[msg.InteractionID] = &simLeg{interactionID: msg.InteractionID, tenantID: msg.TenantID}
	s.mu.Unlock()

	s.emit(ctx, &ProviderEvent{Event: "message.sent", InteractionID: msg.InteractionID, TenantID: msg.TenantID})
	s.emit(ctx, &ProviderEvent{Event: "message.delivered", InteractionID: msg.InteractionID, TenantID: msg.TenantID})
	return nil
}

// emit signs and delivers one provider event through the ingest port. A
// failed delivery retries once after the pace interval — mirroring real
// carrier retry behavior.
func (s *Simulator) emit(ctx context.Context, ev *ProviderEvent) {
	body, err := ev.Encode()
	if err != nil {
		return
	}
	sig := s.signer(body)
	if err := s.ingest(ctx, "simulator", body, sig); err != nil {
		if s.Pace > 0 {
			time.Sleep(s.Pace)
		}
		_ = s.ingest(ctx, "simulator", body, sig)
	}
	if s.Pace > 0 {
		time.Sleep(s.Pace)
	}
}

func (s *Simulator) takeLeg(interactionID string) (*simLeg, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	leg, ok := s.legs[interactionID]
	return leg, ok
}

// typed sentinel errors (adapters wrap into application errors)
type simError string

func (e simError) Error() string { return string(e) }

const (
	errTenantRequired = simError("simulator: tenant context required")
	errUnknownLeg     = simError("simulator: unknown provider leg reference")
)
