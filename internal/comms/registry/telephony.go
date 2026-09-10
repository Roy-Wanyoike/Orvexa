package registry

import (
	"encoding/json"
	"log/slog"
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// TelephonyConfig is the validated credential set for the voice plane. Only
// the selected provider's fields are populated: the env parser never reads
// other providers' variables (least privilege). Hand-built configurations
// (per-tenant overrides) are checked by ValidateTelephony.
type TelephonyConfig struct {
	// Provider is the selected adapter name.
	Provider ProviderName `json:"-"`

	// Twilio Programmable Voice.
	TwilioAccountSID Secret `json:"-"`
	TwilioAuthToken  Secret `json:"-"`
	TwilioFromNumber Secret `json:"-"`

	// Africa's Talking voice.
	ATUsername         Secret `json:"-"`
	ATAPIKey           Secret `json:"-"`
	ATVoiceProductCode Secret `json:"-"`

	// FreeSWITCH Event Socket endpoint. Host/Port are network addressing,
	// not credentials, and render raw; the password never does.
	FreeSwitchHost     string `json:"-"`
	FreeSwitchPort     string `json:"-"`
	FreeSwitchPassword Secret `json:"-"`

	// Asterisk Manager Interface endpoint. The username is half of the AMI
	// auth pair and is redacted conservatively.
	AsteriskHost     string `json:"-"`
	AsteriskPort     string `json:"-"`
	AsteriskUsername Secret `json:"-"`
	AsteriskSecret   Secret `json:"-"`
}

// Optional port defaults applied when the selected provider leaves the port
// env var unset.
const (
	defaultFreeSwitchPort = "8021"
	defaultAsteriskPort   = "5038"
)

// envTargets maps every telephony env var to its struct field. Kept in
// lockstep with the provider specs by the coverage tests.
func (c *TelephonyConfig) envTargets() map[string]*string {
	return map[string]*string{
		"ORVEXA_TWILIO_ACCOUNT_SID":    &c.TwilioAccountSID,
		"ORVEXA_TWILIO_AUTH_TOKEN":     &c.TwilioAuthToken,
		"ORVEXA_TWILIO_FROM_NUMBER":    &c.TwilioFromNumber,
		"ORVEXA_AT_USERNAME":           &c.ATUsername,
		"ORVEXA_AT_API_KEY":            &c.ATAPIKey,
		"ORVEXA_AT_VOICE_PRODUCT_CODE": &c.ATVoiceProductCode,
		"ORVEXA_FREESWITCH_HOST":       &c.FreeSwitchHost,
		"ORVEXA_FREESWITCH_PORT":       &c.FreeSwitchPort,
		"ORVEXA_FREESWITCH_PASSWORD":   &c.FreeSwitchPassword,
		"ORVEXA_ASTERISK_HOST":         &c.AsteriskHost,
		"ORVEXA_ASTERISK_PORT":         &c.AsteriskPort,
		"ORVEXA_ASTERISK_USERNAME":     &c.AsteriskUsername,
		"ORVEXA_ASTERISK_SECRET":       &c.AsteriskSecret,
	}
}

// applyTelephonyDefaults fills optional endpoint defaults for the selected
// provider only — an unselected provider's port must not spring into being.
func applyTelephonyDefaults(c *TelephonyConfig) {
	switch c.Provider {
	case ProviderFreeSwitch:
		if c.FreeSwitchPort == "" {
			c.FreeSwitchPort = defaultFreeSwitchPort
		}
	case ProviderAsterisk:
		if c.AsteriskPort == "" {
			c.AsteriskPort = defaultAsteriskPort
		}
	}
}

// telephonyFromLookup parses the voice-plane configuration from an abstract
// environment. Only the selected provider's env vars are read.
func telephonyFromLookup(get envGetter) (*TelephonyConfig, error) {
	name, err := selectedProvider(get, EnvTelephonyProvider, "telephony", PlaneVoice)
	if err != nil {
		return nil, err
	}
	c := &TelephonyConfig{Provider: name}
	spec, _ := Lookup(name) // known-valid: selectedProvider validated it
	targets := c.envTargets()
	for _, fs := range spec.Telephony {
		if v := strings.TrimSpace(get(fs.Env)); v != "" {
			*targets[fs.Env] = v
		}
	}
	applyTelephonyDefaults(c)
	if err := ValidateTelephony(c); err != nil {
		return nil, err
	}
	return c, nil
}

// ValidateTelephony applies the startup validation contract to any telephony
// configuration, wherever it was built (env, CRM override, tests): unknown
// provider, plane mismatch, and missing required credentials are all
// *apperrors.Error Kind invalid. The missing-credential message lists env
// var names only — never values.
func ValidateTelephony(c *TelephonyConfig) error {
	if c == nil {
		return apperrors.Invalid("comms.config_required", "telephony configuration is required")
	}
	spec, ok := Lookup(c.Provider)
	if !ok {
		return unknownProviderError("telephony provider", string(c.Provider))
	}
	if spec.Planes&PlaneVoice == 0 {
		return unsupportedPlaneError(c.Provider, PlaneVoice)
	}
	targets := c.envTargets()
	var missing []string
	for _, fs := range spec.Telephony {
		if fs.Required && string(*targets[fs.Env]) == "" {
			missing = append(missing, fs.Env)
		}
	}
	if len(missing) > 0 {
		return missingCredentialsError(c.Provider, "telephony", missing)
	}
	return nil
}

// Clone returns an independent copy so resolvers can hand out defensive
// copies without exposing shared mutable state. Nil-safe.
func (c *TelephonyConfig) Clone() *TelephonyConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// redactedFields is the single rendering source for String, LogValue and
// JSON: stable dotted keys, values already redacted. Only the non-credential
// host/port fields render raw, and only when set.
func (c TelephonyConfig) redactedFields() []kv {
	fields := []kv{{"provider", string(c.Provider)}}
	secret := func(key string, v Secret) {
		if v != "" {
			fields = append(fields, kv{key, v.Redacted()})
		}
	}
	plain := func(key, v string) {
		if v != "" {
			fields = append(fields, kv{key, v})
		}
	}
	secret("twilio.account_sid", c.TwilioAccountSID)
	secret("twilio.auth_token", c.TwilioAuthToken)
	secret("twilio.from_number", c.TwilioFromNumber)
	secret("at.username", c.ATUsername)
	secret("at.api_key", c.ATAPIKey)
	secret("at.voice_product_code", c.ATVoiceProductCode)
	plain("freeswitch.host", c.FreeSwitchHost)
	plain("freeswitch.port", c.FreeSwitchPort)
	secret("freeswitch.password", c.FreeSwitchPassword)
	plain("asterisk.host", c.AsteriskHost)
	plain("asterisk.port", c.AsteriskPort)
	secret("asterisk.username", c.AsteriskUsername)
	secret("asterisk.secret", c.AsteriskSecret)
	return fields
}

// String renders the configuration with zero credential material, e.g.
// "telephony(provider=twilio twilio.account_sid=****0aa1 twilio.auth_token=****cdef)".
func (c TelephonyConfig) String() string { return renderKV("telephony", c.redactedFields()) }

// GoString renders the masked form so %#v cannot disclose credentials.
func (c TelephonyConfig) GoString() string { return c.String() }

// LogValue renders the masked form for structured logging.
func (c TelephonyConfig) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON renders only redacted fields; the struct's own fields are
// tagged json:"-" so unmarshaling can never smuggle credentials in either.
func (c TelephonyConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(kvMap(c.redactedFields()))
}
