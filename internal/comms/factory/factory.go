// Package factory is the single place where Orvexa turns validated provider
// registry configuration into live telephony/messaging adapters: the
// registry → adapters → cmd/api wiring ([O-23], issue #32).
//
// # Placement
//
// This is a subpackage of internal/comms rather than a file inside it
// deliberately: every provider adapter imports internal/comms (for
// comms.IngestFunc, comms.Signer and comms.ProviderEvent), so a factory
// living in package comms itself would create a compile-time import cycle
// (comms → adapters → comms). The subpackage keeps one direction: factory →
// adapters → comms.
//
// # Contract
//
// Build consumes the validated registry configurations produced by
// registry.LoadFromEnv (or hand-built configurations already checked by
// registry.ValidateTelephony/ValidateMessaging) and constructs one provider
// per plane:
//
//	telephony (ORVEXA_TELEPHONY_PROVIDER)   messaging (ORVEXA_MESSAGING_PROVIDER)
//	  simulator (default)                     simulator (default)
//	  twilio                                  twilio
//	  africastalking                          whatsappcloud
//	  freeswitch                              africastalking
//	  asterisk
//
// Unknown or misconfigured providers are startup errors — the registry
// validation errors name the offending environment variables (never their
// values) — and adapter constructor failures surface wrapped with the
// provider name for context.
//
// # Eventing parity with the simulator
//
// Every constructed adapter reports lifecycle progress exactly like the
// built-in simulator: by delivering signed comms.ProviderEvent webhooks
// through the comms.IngestFunc port. Production wiring hands Build the same
// ingest function and signer the simulator uses — the fail-closed webhook
// gateway (webhooks.Gateway.Ingest) and webhooks.ComputeSignature — so no
// provider ever bypasses signature validation.
//
// # Honest simulator selection
//
// A plane configured as (or left unset, which the registry resolves to) the
// simulator boots with the built-in deterministic carrier. LogSelection
// renders that choice explicitly — provider=simulator reason=not_configured
// — so a simulator boot can never be mistaken for a configured carrier.
//
// # Teardown
//
// Build returns a teardown func that releases provider resources on
// shutdown. Session-based adapters (FreeSWITCH ESL, Asterisk AMI) close
// their connections; pure-REST adapters need nothing and contribute a no-op.
// Teardown is safe to call more than once: the adapters guard their own
// close paths with sync.Once.
//
// # Layering
//
// factory imports the registry, the two plane cores (ports) and the adapter
// packages — and nothing above them. The domain core never sees provider
// names or credentials; credentials flow from the registry into adapter
// configs and are rendered only through the adapters' redacted paths.
package factory

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	atmsg "github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/africastalking"
	twiliomsg "github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/twilio"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/whatsappcloud"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	atvoice "github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/africastalking"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/asterisk"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/freeswitch"
	twiliovoice "github.com/Roy-Wanyoike/orvexa/internal/telephony/adapters/twilio"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Compile-time proof that every provider Build can return satisfies its
// plane's port — including the simulator, so a factory change can never
// silently unsatisfy the hexagonal contract.
var (
	_ telephony.VoiceProvider     = (*comms.Simulator)(nil)
	_ telephony.VoiceProvider     = (*twiliovoice.Adapter)(nil)
	_ telephony.VoiceProvider     = (*atvoice.Adapter)(nil)
	_ telephony.VoiceProvider     = (*freeswitch.Adapter)(nil)
	_ telephony.VoiceProvider     = (*asterisk.Voice)(nil)
	_ messaging.MessagingProvider = (*comms.Simulator)(nil)
	_ messaging.MessagingProvider = (*twiliomsg.Adapter)(nil)
	_ messaging.MessagingProvider = (*whatsappcloud.Provider)(nil)
	_ messaging.MessagingProvider = (*atmsg.Adapter)(nil)
)

// defaultFreeSwitchPort mirrors the registry's optional-port default. The
// registry applies it when parsing ORVEXA_FREESWITCH_PORT from the
// environment; the factory defends the same value for hand-built
// configurations that left the port empty.
const defaultFreeSwitchPort = "8021"

// Config is the factory's construction contract.
type Config struct {
	// Telephony is the validated voice-plane registry configuration. nil
	// selects the built-in simulator (the zero-dependency boot posture; the
	// honesty of that default is preserved by LogSelection).
	Telephony *registry.TelephonyConfig

	// Messaging is the validated messaging-plane registry configuration.
	// nil selects the built-in simulator.
	Messaging *registry.MessagingConfig

	// Ingest delivers signed provider webhooks into the platform
	// (comms.IngestFunc). Required — production wiring passes the webhook
	// gateway's Ingest, the exact same entry point the simulator uses.
	Ingest comms.IngestFunc

	// Signer signs every provider webhook body (comms.Signer). Required —
	// the webhook gateway is fail-closed and drops unsigned traffic.
	Signer comms.Signer

	// Logger receives structured, credential-free adapter operational logs.
	// Adapters that accept a logger get it; adapters without a logger knob
	// stay silent. nil = silent/default per adapter contract.
	Logger *slog.Logger

	// Twilio deployment endpoints are application URLs, not credentials, so
	// they live outside the credential registry. Zero values leave the
	// adapters at their documented defaults: Twilio voice PlaceCall/Hold
	// fail with typed errors naming the missing knob until the endpoints
	// are provisioned (see internal/telephony/adapters/twilio README);
	// Twilio messaging sends succeed fire-and-forget without receipts.
	TwilioVoiceTwiMLBaseURL     string
	TwilioVoiceCallbackBaseURL  string
	TwilioVoiceHoldTwiMLURL     string
	TwilioVoiceResumeURL        string
	TwilioMsgStatusCallbackBase string
}

// Build constructs the configured provider for each plane and returns the
// two plane providers plus a teardown func that releases provider resources
// (safe to call more than once). Construction never blocks on the network:
// session-based adapters (FreeSWITCH, Asterisk) dial and authenticate in
// their own background supervisors, and their first command waits within
// its own timeout budget.
//
// Misconfiguration is a startup error carrying an actionable message: the
// registry validation errors name the environment variables to set (never
// their values), and adapter constructor failures are wrapped with the
// provider name.
func Build(ctx context.Context, cfg Config) (telephony.VoiceProvider, messaging.MessagingProvider, func(), error) {
	if cfg.Ingest == nil || cfg.Signer == nil {
		return nil, nil, nil, apperrors.Invalid("comms.ingest_not_wired",
			"provider factory requires Ingest (comms.IngestFunc) and Signer (comms.Signer): "+
				"every provider, including the simulator, delivers signed webhooks through the fail-closed gateway")
	}

	var teardowns []func()

	// The simulator is constructed lazily and at most once per Build: when
	// both planes run on it, one instance serves both ports (matching the
	// pre-factory wiring); when only one plane does, the other plane's real
	// adapter simply never touches it.
	var sim *comms.Simulator
	simulator := func() *comms.Simulator {
		if sim == nil {
			sim = comms.NewSimulator(cfg.Ingest, cfg.Signer, 0)
		}
		return sim
	}

	// ---- voice plane ----
	var voice telephony.VoiceProvider
	telProvider := registry.ProviderSimulator
	if cfg.Telephony != nil {
		if err := registry.ValidateTelephony(cfg.Telephony); err != nil {
			return nil, nil, nil, err
		}
		telProvider = cfg.Telephony.Provider
	}
	switch telProvider {
	case registry.ProviderSimulator:
		voice = simulator()
	case registry.ProviderTwilio:
		tc := cfg.Telephony
		voice = twiliovoice.New(twiliovoice.Config{
			AccountSID:      tc.TwilioAccountSID,
			AuthToken:       tc.TwilioAuthToken,
			TwiMLBaseURL:    cfg.TwilioVoiceTwiMLBaseURL,
			CallbackBaseURL: cfg.TwilioVoiceCallbackBaseURL,
			HoldTwiMLURL:    cfg.TwilioVoiceHoldTwiMLURL,
			ResumeURL:       cfg.TwilioVoiceResumeURL,
			Ingest:          cfg.Ingest,
			Signer:          cfg.Signer,
		})
	case registry.ProviderAfricasTalking:
		tc := cfg.Telephony
		a, err := atvoice.New(atvoice.Config{
			Username: tc.ATUsername,
			APIKey:   tc.ATAPIKey,
			Ingest:   cfg.Ingest,
			Signer:   cfg.Signer,
			Logger:   cfg.Logger,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telephony provider %q: %w", telProvider, err)
		}
		voice = a
	case registry.ProviderFreeSwitch:
		tc := cfg.Telephony
		port := tc.FreeSwitchPort
		if port == "" {
			port = defaultFreeSwitchPort
		}
		a, err := freeswitch.New(freeswitch.Config{
			Addr:     net.JoinHostPort(tc.FreeSwitchHost, port),
			Password: tc.FreeSwitchPassword,
			Ingest:   cfg.Ingest,
			Signer:   cfg.Signer, // platform HMAC signer: the gateway is fail-closed
			Logger:   cfg.Logger,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telephony provider %q: %w", telProvider, err)
		}
		voice = a
		teardowns = append(teardowns, a.Close)
	case registry.ProviderAsterisk:
		ac, err := asterisk.FromRegistry(cfg.Telephony)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telephony provider %q: %w", telProvider, err)
		}
		// The AMI adapter's ingest hook predates the context-carrying
		// comms.IngestFunc signature; adapt without losing the delivery
		// semantics (the gateway owns deadlines/pool lifetimes).
		ac.Ingest = func(provider string, body []byte, signature string) error {
			return cfg.Ingest(context.Background(), provider, body, signature)
		}
		ac.Signer = cfg.Signer
		ac.Logger = cfg.Logger
		v, err := asterisk.New(ac)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("telephony provider %q: %w", telProvider, err)
		}
		voice = v
		teardowns = append(teardowns, v.Close)
	default:
		return nil, nil, nil, apperrors.Invalid("comms.unknown_provider",
			fmt.Sprintf("no telephony constructor for provider %q (registered providers: %v)", telProvider, registry.Names()))
	}

	// ---- messaging plane ----
	var messenger messaging.MessagingProvider
	msgProvider := registry.ProviderSimulator
	if cfg.Messaging != nil {
		if err := registry.ValidateMessaging(cfg.Messaging); err != nil {
			return nil, nil, nil, err
		}
		msgProvider = cfg.Messaging.Provider
	}
	switch msgProvider {
	case registry.ProviderSimulator:
		messenger = simulator()
	case registry.ProviderTwilio:
		mc := cfg.Messaging
		a, err := twiliomsg.New(twiliomsg.Config{
			AccountSID:         mc.TwilioAccountSID,
			AuthToken:          mc.TwilioAuthToken,
			FromNumber:         mc.TwilioFromNumber,
			StatusCallbackBase: cfg.TwilioMsgStatusCallbackBase,
			Ingest:             cfg.Ingest,
			Signer:             cfg.Signer,
			Logger:             cfg.Logger,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("messaging provider %q: %w", msgProvider, err)
		}
		messenger = a
	case registry.ProviderWhatsAppCloud:
		mc := cfg.Messaging
		p, err := whatsappcloud.New(whatsappcloud.Config{
			PhoneNumberID: string(mc.WhatsAppPhoneNumberID),
			AccessToken:   mc.WhatsAppAccessToken,
			Ingest:        cfg.Ingest,
			Signer:        cfg.Signer,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("messaging provider %q: %w", msgProvider, err)
		}
		messenger = p
	case registry.ProviderAfricasTalking:
		mc := cfg.Messaging
		a, err := atmsg.New(atmsg.Config{
			Username: mc.ATUsername,
			APIKey:   mc.ATAPIKey,
			SenderID: mc.ATSenderID,
			Ingest:   cfg.Ingest,
			Signer:   cfg.Signer,
			Logger:   cfg.Logger,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("messaging provider %q: %w", msgProvider, err)
		}
		messenger = a
	default:
		return nil, nil, nil, apperrors.Invalid("comms.unknown_provider",
			fmt.Sprintf("no messaging constructor for provider %q (registered providers: %v)", msgProvider, registry.Names()))
	}

	teardown := func() {
		for i := len(teardowns) - 1; i >= 0; i-- {
			teardowns[i]()
		}
	}
	return voice, messenger, teardown, nil
}

// LogSelection emits the honest provider-selection startup line for one
// plane. A real adapter logs provider=<name>; the built-in simulator logs
// provider=simulator reason=not_configured so a simulator boot is asserted
// in the operator's face instead of silently standing in for a carrier.
func LogSelection(log *slog.Logger, plane, provider string) {
	if log == nil {
		return
	}
	if provider == string(registry.ProviderSimulator) {
		log.Info("comms provider selected", "plane", plane, "provider", "simulator", "reason", "not_configured")
		return
	}
	log.Info("comms provider selected", "plane", plane, "provider", provider)
}
