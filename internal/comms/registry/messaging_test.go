package registry

import (
	"errors"
	"strings"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func TestMessagingFromLookup(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		wantCode    string // "" = must succeed
		wantMissing string // substring of the missing-credential message
		check       func(*testing.T, *MessagingConfig)
	}{
		{
			name: "unset provider defaults to simulator",
			env:  nil,
			check: func(t *testing.T, c *MessagingConfig) {
				t.Helper()
				if c.Provider != ProviderSimulator {
					t.Fatalf("provider = %q, want simulator", c.Provider)
				}
			},
		},
		{
			name: "whatsappcloud happy path reads all four credentials",
			env: withFakeEnv(map[string]string{
				EnvMessagingProvider: "whatsappcloud",
			}),
			check: func(t *testing.T, c *MessagingConfig) {
				t.Helper()
				if c.WhatsAppPhoneNumberID != Secret(fakeProviderEnv["ORVEXA_WHATSAPP_PHONE_NUMBER_ID"]) ||
					c.WhatsAppAccessToken != Secret(fakeProviderEnv["ORVEXA_WHATSAPP_ACCESS_TOKEN"]) ||
					c.WhatsAppAppSecret != Secret(fakeProviderEnv["ORVEXA_WHATSAPP_APP_SECRET"]) ||
					c.WhatsAppVerifyToken != Secret(fakeProviderEnv["ORVEXA_WHATSAPP_VERIFY_TOKEN"]) {
					t.Fatal("whatsapp cloud credentials must round-trip into the typed fields")
				}
				if c.TwilioAccountSID != "" || c.ATSenderID != "" {
					t.Fatal("unselected provider credentials must not be read")
				}
			},
		},
		{
			name:        "whatsappcloud missing app secret and verify token lists both",
			env:         map[string]string{EnvMessagingProvider: "whatsappcloud"},
			wantCode:    "comms.missing_credentials",
			wantMissing: "ORVEXA_WHATSAPP_PHONE_NUMBER_ID, ORVEXA_WHATSAPP_ACCESS_TOKEN, ORVEXA_WHATSAPP_APP_SECRET, ORVEXA_WHATSAPP_VERIFY_TOKEN",
		},
		{
			name: "twilio messaging happy path",
			env: withFakeEnv(map[string]string{
				EnvMessagingProvider: "twilio",
			}),
			check: func(t *testing.T, c *MessagingConfig) {
				t.Helper()
				if c.TwilioAccountSID != Secret(fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]) ||
					c.TwilioAuthToken != Secret(fakeProviderEnv["ORVEXA_TWILIO_AUTH_TOKEN"]) ||
					c.TwilioFromNumber != Secret(fakeProviderEnv["ORVEXA_TWILIO_FROM_NUMBER"]) {
					t.Fatal("twilio messaging credentials must round-trip")
				}
			},
		},
		{
			name: "africastalking messaging happy path",
			env: withFakeEnv(map[string]string{
				EnvMessagingProvider: "africastalking",
			}),
			check: func(t *testing.T, c *MessagingConfig) {
				t.Helper()
				if c.ATUsername != Secret(fakeProviderEnv["ORVEXA_AT_USERNAME"]) ||
					c.ATAPIKey != Secret(fakeProviderEnv["ORVEXA_AT_API_KEY"]) ||
					c.ATSenderID != Secret(fakeProviderEnv["ORVEXA_AT_SENDER_ID"]) {
					t.Fatal("africa's talking messaging credentials must round-trip")
				}
			},
		},
		{
			name:        "africastalking missing sender id lists it",
			env:         map[string]string{EnvMessagingProvider: "africastalking"},
			wantCode:    "comms.missing_credentials",
			wantMissing: "ORVEXA_AT_USERNAME, ORVEXA_AT_API_KEY, ORVEXA_AT_SENDER_ID",
		},
		{
			name:     "freeswitch cannot serve the messaging plane",
			env:      map[string]string{EnvMessagingProvider: "freeswitch"},
			wantCode: "comms.unsupported_plane",
		},
		{
			name:     "asterisk cannot serve the messaging plane",
			env:      map[string]string{EnvMessagingProvider: "asterisk"},
			wantCode: "comms.unsupported_plane",
		},
		{
			name:     "unknown messaging provider",
			env:      map[string]string{EnvMessagingProvider: "plivo"},
			wantCode: "comms.unknown_provider",
		},
		{
			name: "messaging env does not influence telephony parse",
			env: withFakeEnv(map[string]string{
				EnvMessagingProvider: "twilio",
			}),
			check: func(t *testing.T, c *MessagingConfig) {
				t.Helper()
				if c.Provider != ProviderTwilio {
					t.Fatalf("provider = %q, want twilio", c.Provider)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := messagingFromLookup(lookupFrom(tc.env))
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("parse must succeed: %v", err)
				}
				if err := ValidateMessaging(c); err != nil {
					t.Fatalf("parsed config must validate: %v", err)
				}
				if tc.check != nil {
					tc.check(t, c)
				}
				return
			}
			var appErr *apperrors.Error
			if !errors.As(err, &appErr) {
				t.Fatalf("want *apperrors.Error, got %v", err)
			}
			if appErr.Kind != apperrors.KindInvalid {
				t.Errorf("kind = %q, want invalid", appErr.Kind)
			}
			if appErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", appErr.Code, tc.wantCode)
			}
			if tc.wantMissing != "" && !strings.Contains(appErr.Message, tc.wantMissing) {
				t.Errorf("message %q must list the missing vars %q", appErr.Message, tc.wantMissing)
			}
		})
	}
}

func TestValidateMessagingHandBuilt(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		err := ValidateMessaging(nil)
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.config_required" {
			t.Fatalf("want comms.config_required, got %v", err)
		}
	})
	t.Run("plane mismatch", func(t *testing.T) {
		err := ValidateMessaging(&MessagingConfig{Provider: ProviderFreeSwitch})
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.unsupported_plane" {
			t.Fatalf("want comms.unsupported_plane, got %v", err)
		}
	})
	t.Run("complete config passes", func(t *testing.T) {
		c := &MessagingConfig{
			Provider:              ProviderWhatsAppCloud,
			WhatsAppPhoneNumberID: Secret(fakeProviderEnv["ORVEXA_WHATSAPP_PHONE_NUMBER_ID"]),
			WhatsAppAccessToken:   Secret(fakeProviderEnv["ORVEXA_WHATSAPP_ACCESS_TOKEN"]),
			WhatsAppAppSecret:     Secret(fakeProviderEnv["ORVEXA_WHATSAPP_APP_SECRET"]),
			WhatsAppVerifyToken:   Secret(fakeProviderEnv["ORVEXA_WHATSAPP_VERIFY_TOKEN"]),
		}
		if err := ValidateMessaging(c); err != nil {
			t.Fatalf("complete config must validate: %v", err)
		}
	})
}

func TestMessagingCloneIsIndependent(t *testing.T) {
	c := &MessagingConfig{Provider: ProviderTwilio, TwilioFromNumber: "f4ke"}
	cp := c.Clone()
	cp.TwilioFromNumber = "other"
	if c.TwilioFromNumber != "f4ke" {
		t.Fatal("clone mutation leaked into the original")
	}
	var nilConfig *MessagingConfig
	if nilConfig.Clone() != nil {
		t.Fatal("nil clone must stay nil")
	}
}
