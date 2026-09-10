package registry

import (
	"encoding/json"
	"log/slog"
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// MessagingConfig is the validated credential set for the messaging plane.
// Only the selected provider's fields are populated: the env parser never
// reads other providers' variables (least privilege). Hand-built
// configurations (per-tenant overrides) are checked by ValidateMessaging.
type MessagingConfig struct {
	// Provider is the selected adapter name.
	Provider ProviderName `json:"-"`

	// Twilio Messaging (SMS).
	TwilioAccountSID Secret `json:"-"`
	TwilioAuthToken  Secret `json:"-"`
	TwilioFromNumber Secret `json:"-"`

	// WhatsApp Cloud API (Meta Graph). The app secret verifies webhook
	// signatures; the verify token completes the webhook subscription
	// handshake.
	WhatsAppPhoneNumberID Secret `json:"-"`
	WhatsAppAccessToken   Secret `json:"-"`
	WhatsAppAppSecret     Secret `json:"-"`
	WhatsAppVerifyToken   Secret `json:"-"`

	// Africa's Talking messaging.
	ATUsername Secret `json:"-"`
	ATAPIKey   Secret `json:"-"`
	ATSenderID Secret `json:"-"`
}

// envTargets maps every messaging env var to its struct field; every
// messaging field is credential material, so all targets are secretRef.
// Kept in lockstep with the provider specs by the coverage tests.
func (c *MessagingConfig) envTargets() map[string]fieldRef {
	return map[string]fieldRef{
		"ORVEXA_TWILIO_ACCOUNT_SID":       secretRef(&c.TwilioAccountSID),
		"ORVEXA_TWILIO_AUTH_TOKEN":        secretRef(&c.TwilioAuthToken),
		"ORVEXA_TWILIO_FROM_NUMBER":       secretRef(&c.TwilioFromNumber),
		"ORVEXA_WHATSAPP_PHONE_NUMBER_ID": secretRef(&c.WhatsAppPhoneNumberID),
		"ORVEXA_WHATSAPP_ACCESS_TOKEN":    secretRef(&c.WhatsAppAccessToken),
		"ORVEXA_WHATSAPP_APP_SECRET":      secretRef(&c.WhatsAppAppSecret),
		"ORVEXA_WHATSAPP_VERIFY_TOKEN":    secretRef(&c.WhatsAppVerifyToken),
		"ORVEXA_AT_USERNAME":              secretRef(&c.ATUsername),
		"ORVEXA_AT_API_KEY":               secretRef(&c.ATAPIKey),
		"ORVEXA_AT_SENDER_ID":             secretRef(&c.ATSenderID),
	}
}

// messagingFromLookup parses the messaging-plane configuration from an
// abstract environment. Only the selected provider's env vars are read.
func messagingFromLookup(get envGetter) (*MessagingConfig, error) {
	name, err := selectedProvider(get, EnvMessagingProvider, "messaging", PlaneMessaging)
	if err != nil {
		return nil, err
	}
	c := &MessagingConfig{Provider: name}
	spec, _ := Lookup(name) // known-valid: selectedProvider validated it
	targets := c.envTargets()
	for _, fs := range spec.Messaging {
		if v := strings.TrimSpace(get(fs.Env)); v != "" {
			targets[fs.Env].set(v)
		}
	}
	if err := ValidateMessaging(c); err != nil {
		return nil, err
	}
	return c, nil
}

// ValidateMessaging applies the startup validation contract to any messaging
// configuration, wherever it was built (env, CRM override, tests): unknown
// provider, plane mismatch, and missing required credentials are all
// *apperrors.Error Kind invalid. The missing-credential message lists env
// var names only — never values.
func ValidateMessaging(c *MessagingConfig) error {
	if c == nil {
		return apperrors.Invalid("comms.config_required", "messaging configuration is required")
	}
	spec, ok := Lookup(c.Provider)
	if !ok {
		return unknownProviderError("messaging provider", string(c.Provider))
	}
	if spec.Planes&PlaneMessaging == 0 {
		return unsupportedPlaneError(c.Provider, PlaneMessaging)
	}
	targets := c.envTargets()
	var missing []string
	for _, fs := range spec.Messaging {
		if fs.Required && targets[fs.Env].get() == "" {
			missing = append(missing, fs.Env)
		}
	}
	if len(missing) > 0 {
		return missingCredentialsError(c.Provider, "messaging", missing)
	}
	return nil
}

// Clone returns an independent copy so resolvers can hand out defensive
// copies without exposing shared mutable state. Nil-safe.
func (c *MessagingConfig) Clone() *MessagingConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// redactedFields is the single rendering source for String, LogValue and
// JSON: stable dotted keys, every value redacted.
func (c MessagingConfig) redactedFields() []kv {
	fields := []kv{{"provider", string(c.Provider)}}
	secret := func(key string, v Secret) {
		if v != "" {
			fields = append(fields, kv{key, v.Redacted()})
		}
	}
	secret("twilio.account_sid", c.TwilioAccountSID)
	secret("twilio.auth_token", c.TwilioAuthToken)
	secret("twilio.from_number", c.TwilioFromNumber)
	secret("whatsapp.phone_number_id", c.WhatsAppPhoneNumberID)
	secret("whatsapp.access_token", c.WhatsAppAccessToken)
	secret("whatsapp.app_secret", c.WhatsAppAppSecret)
	secret("whatsapp.verify_token", c.WhatsAppVerifyToken)
	secret("at.username", c.ATUsername)
	secret("at.api_key", c.ATAPIKey)
	secret("at.sender_id", c.ATSenderID)
	return fields
}

// String renders the configuration with zero credential material, e.g.
// "messaging(provider=whatsappcloud whatsapp.access_token=****ff66)".
func (c MessagingConfig) String() string { return renderKV("messaging", c.redactedFields()) }

// GoString renders the masked form so %#v cannot disclose credentials.
func (c MessagingConfig) GoString() string { return c.String() }

// LogValue renders the masked form for structured logging.
func (c MessagingConfig) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON renders only redacted fields; the struct's own fields are
// tagged json:"-" so unmarshaling can never smuggle credentials in either.
func (c MessagingConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(kvMap(c.redactedFields()))
}
