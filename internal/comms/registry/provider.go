package registry

import (
	"fmt"
	"sort"
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ProviderName identifies a communications provider adapter. Canonical names
// are lowercase and matched exactly (no case folding, no trimming at this
// layer — env parsing trims before lookup) so a mis-typed provider name fails
// loudly at startup instead of silently rerouting traffic.
type ProviderName string

const (
	// ProviderSimulator is the built-in deterministic carrier. It performs
	// no network I/O and requires no credentials — the zero-dependency boot
	// posture of the platform.
	ProviderSimulator ProviderName = "simulator"
	// ProviderTwilio selects the Twilio adapter (voice + messaging).
	ProviderTwilio ProviderName = "twilio"
	// ProviderWhatsAppCloud selects the WhatsApp Cloud API adapter (messaging).
	ProviderWhatsAppCloud ProviderName = "whatsappcloud"
	// ProviderAfricasTalking selects the Africa's Talking adapter (voice + messaging).
	ProviderAfricasTalking ProviderName = "africastalking"
	// ProviderFreeSwitch selects the self-hosted FreeSWITCH adapter (voice).
	ProviderFreeSwitch ProviderName = "freeswitch"
	// ProviderAsterisk selects the self-hosted Asterisk adapter (voice).
	ProviderAsterisk ProviderName = "asterisk"
)

// String returns the canonical provider name.
func (p ProviderName) String() string { return string(p) }

// Plane identifies a communications plane a provider can serve.
type Plane uint8

const (
	PlaneVoice Plane = 1 << iota
	PlaneMessaging
)

// String renders the plane set, e.g. "voice", "messaging", "voice+messaging".
func (p Plane) String() string {
	parts := make([]string, 0, 2)
	if p&PlaneVoice != 0 {
		parts = append(parts, "voice")
	}
	if p&PlaneMessaging != 0 {
		parts = append(parts, "messaging")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// FieldSpec describes one entry of a provider's credential shape: the
// environment variable that feeds it, whether it is required for the plane,
// and whether it is credential material (secret fields are redacted on every
// rendering path; non-secret endpoint fields such as host/port render raw).
type FieldSpec struct {
	Env      string
	Required bool
	Secret   bool
}

// ProviderSpec is the registry entry for one provider: the planes it serves,
// a human summary, and the credential shape per plane. Treat returned specs
// as read-only.
type ProviderSpec struct {
	Name      ProviderName
	Planes    Plane
	Summary   string
	Telephony []FieldSpec // voice-plane credential shape; nil if the plane is unsupported
	Messaging []FieldSpec // messaging-plane credential shape; nil if the plane is unsupported
}

// providerRegistry is the closed set of supported providers. It is the single
// source of truth for provider names and credential shapes in Orvexa.
var providerRegistry = map[ProviderName]ProviderSpec{
	ProviderSimulator: {
		Name:    ProviderSimulator,
		Planes:  PlaneVoice | PlaneMessaging,
		Summary: "built-in deterministic carrier; no network I/O; no credentials",
	},
	ProviderTwilio: {
		Name:    ProviderTwilio,
		Planes:  PlaneVoice | PlaneMessaging,
		Summary: "Twilio Programmable Voice and Messaging",
		Telephony: []FieldSpec{
			{Env: "ORVEXA_TWILIO_ACCOUNT_SID", Required: true, Secret: true},
			{Env: "ORVEXA_TWILIO_AUTH_TOKEN", Required: true, Secret: true},
			{Env: "ORVEXA_TWILIO_FROM_NUMBER", Required: true, Secret: true},
		},
		Messaging: []FieldSpec{
			{Env: "ORVEXA_TWILIO_ACCOUNT_SID", Required: true, Secret: true},
			{Env: "ORVEXA_TWILIO_AUTH_TOKEN", Required: true, Secret: true},
			{Env: "ORVEXA_TWILIO_FROM_NUMBER", Required: true, Secret: true},
		},
	},
	ProviderWhatsAppCloud: {
		Name:    ProviderWhatsAppCloud,
		Planes:  PlaneMessaging,
		Summary: "WhatsApp Cloud API (Meta Graph)",
		Messaging: []FieldSpec{
			{Env: "ORVEXA_WHATSAPP_PHONE_NUMBER_ID", Required: true, Secret: true},
			{Env: "ORVEXA_WHATSAPP_ACCESS_TOKEN", Required: true, Secret: true},
			{Env: "ORVEXA_WHATSAPP_APP_SECRET", Required: true, Secret: true},
			{Env: "ORVEXA_WHATSAPP_VERIFY_TOKEN", Required: true, Secret: true},
		},
	},
	ProviderAfricasTalking: {
		Name:    ProviderAfricasTalking,
		Planes:  PlaneVoice | PlaneMessaging,
		Summary: "Africa's Talking voice and messaging",
		Telephony: []FieldSpec{
			{Env: "ORVEXA_AT_USERNAME", Required: true, Secret: true},
			{Env: "ORVEXA_AT_API_KEY", Required: true, Secret: true},
			{Env: "ORVEXA_AT_VOICE_PRODUCT_CODE", Required: true, Secret: true},
		},
		Messaging: []FieldSpec{
			{Env: "ORVEXA_AT_USERNAME", Required: true, Secret: true},
			{Env: "ORVEXA_AT_API_KEY", Required: true, Secret: true},
			// Fail-closed: without a registered sender ID many AT accounts
			// fail per-send at runtime; a boot-time error is cheaper to
			// diagnose. Relaxing this is additive.
			{Env: "ORVEXA_AT_SENDER_ID", Required: true, Secret: true},
		},
	},
	ProviderFreeSwitch: {
		Name:    ProviderFreeSwitch,
		Planes:  PlaneVoice,
		Summary: "self-hosted FreeSWITCH (Event Socket)",
		Telephony: []FieldSpec{
			{Env: "ORVEXA_FREESWITCH_HOST", Required: true},
			{Env: "ORVEXA_FREESWITCH_PORT"}, // optional; defaults to 8021
			{Env: "ORVEXA_FREESWITCH_PASSWORD", Required: true, Secret: true},
		},
	},
	ProviderAsterisk: {
		Name:    ProviderAsterisk,
		Planes:  PlaneVoice,
		Summary: "self-hosted Asterisk (Manager Interface)",
		Telephony: []FieldSpec{
			{Env: "ORVEXA_ASTERISK_HOST", Required: true},
			{Env: "ORVEXA_ASTERISK_PORT"}, // optional; defaults to 5038
			// Username is half of the AMI auth pair; redacted conservatively.
			{Env: "ORVEXA_ASTERISK_USERNAME", Required: true, Secret: true},
			{Env: "ORVEXA_ASTERISK_SECRET", Required: true, Secret: true},
		},
	},
}

// Lookup returns the registry entry for name.
func Lookup(name ProviderName) (ProviderSpec, bool) {
	spec, ok := providerRegistry[name]
	return spec, ok
}

// Names returns every registered provider name in sorted order.
func Names() []ProviderName {
	out := make([]ProviderName, 0, len(providerRegistry))
	for name := range providerRegistry {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Validate reports whether name is a registered provider. Unknown names yield
// an *apperrors.Error with Kind invalid and code "comms.unknown_provider";
// the message lists the registered names (never any credential material).
func Validate(name ProviderName) error {
	if _, ok := providerRegistry[name]; !ok {
		return unknownProviderError("provider", string(name))
	}
	return nil
}

// ValidateForPlane additionally requires that the provider serves plane.
func ValidateForPlane(name ProviderName, p Plane) error {
	spec, ok := providerRegistry[name]
	if !ok {
		return unknownProviderError("provider", string(name))
	}
	if spec.Planes&p == 0 {
		return unsupportedPlaneError(name, p)
	}
	return nil
}

func unknownProviderError(label, raw string) error {
	return apperrors.Invalid("comms.unknown_provider",
		fmt.Sprintf("unknown %s %q (registered providers: %s)", label, raw, strings.Join(nameStrings(), ", ")))
}

func unsupportedPlaneError(name ProviderName, p Plane) error {
	return apperrors.Invalid("comms.unsupported_plane",
		fmt.Sprintf("provider %q does not support the %s plane (supports: %s)", name, p, providerRegistry[name].Planes))
}

func missingCredentialsError(name ProviderName, plane string, missing []string) error {
	return apperrors.Invalid("comms.missing_credentials",
		fmt.Sprintf("provider %q (%s) is missing required environment variable(s): %s",
			name, plane, strings.Join(missing, ", ")))
}

func nameStrings() []string {
	names := Names()
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return out
}
