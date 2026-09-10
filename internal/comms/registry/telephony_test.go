package registry

import (
	"errors"
	"strings"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func TestTelephonyFromLookup(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		wantCode    string // "" = must succeed
		wantMissing string // substring of the missing-credential message
		check       func(*testing.T, *TelephonyConfig)
	}{
		{
			name: "unset provider defaults to simulator",
			env:  nil,
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.Provider != ProviderSimulator {
					t.Fatalf("provider = %q, want simulator", c.Provider)
				}
			},
		},
		{
			name: "empty provider value defaults to simulator",
			env:  map[string]string{EnvTelephonyProvider: "   "},
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.Provider != ProviderSimulator {
					t.Fatalf("provider = %q, want simulator", c.Provider)
				}
			},
		},
		{
			name: "twilio happy path reads only twilio variables",
			env:  withFakeEnv(map[string]string{EnvTelephonyProvider: "twilio"}),
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.TwilioAccountSID != Secret(fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]) ||
					c.TwilioAuthToken != Secret(fakeProviderEnv["ORVEXA_TWILIO_AUTH_TOKEN"]) ||
					c.TwilioFromNumber != Secret(fakeProviderEnv["ORVEXA_TWILIO_FROM_NUMBER"]) {
					t.Fatal("twilio credentials must round-trip into the typed fields")
				}
				// Least privilege: other providers' env vars are ignored.
				if c.ATUsername != "" || c.ATAPIKey != "" || c.ATVoiceProductCode != "" ||
					c.FreeSwitchHost != "" || c.FreeSwitchPort != "" || c.FreeSwitchPassword != "" ||
					c.AsteriskHost != "" || c.AsteriskPort != "" || c.AsteriskUsername != "" || c.AsteriskSecret != "" {
					t.Fatal("unselected provider credentials must not be read")
				}
				// Defaults apply only to the selected provider.
				if c.FreeSwitchPort != "" {
					t.Fatal("freeswitch port default must not leak into a twilio config")
				}
			},
		},
		{
			name:        "twilio missing token and number lists both in order",
			env:         map[string]string{EnvTelephonyProvider: "twilio", "ORVEXA_TWILIO_ACCOUNT_SID": fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]},
			wantCode:    "comms.missing_credentials",
			wantMissing: "ORVEXA_TWILIO_AUTH_TOKEN, ORVEXA_TWILIO_FROM_NUMBER",
		},
		{
			name:        "twilio missing everything lists all three",
			env:         map[string]string{EnvTelephonyProvider: "twilio"},
			wantCode:    "comms.missing_credentials",
			wantMissing: "ORVEXA_TWILIO_ACCOUNT_SID, ORVEXA_TWILIO_AUTH_TOKEN, ORVEXA_TWILIO_FROM_NUMBER",
		},
		{
			name:     "whatsappcloud cannot serve the voice plane",
			env:      map[string]string{EnvTelephonyProvider: "whatsappcloud"},
			wantCode: "comms.unsupported_plane",
		},
		{
			name:        "unknown telephony provider",
			env:         map[string]string{EnvTelephonyProvider: "plivo"},
			wantCode:    "comms.unknown_provider",
			wantMissing: "",
		},
		{
			name: "freeswitch port defaults when unset",
			env: map[string]string{
				EnvTelephonyProvider:         "freeswitch",
				"ORVEXA_FREESWITCH_HOST":     fakeProviderEnv["ORVEXA_FREESWITCH_HOST"],
				"ORVEXA_FREESWITCH_PASSWORD": fakeProviderEnv["ORVEXA_FREESWITCH_PASSWORD"],
			},
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.FreeSwitchPort != defaultFreeSwitchPort {
					t.Fatalf("freeswitch port = %q, want default %q", c.FreeSwitchPort, defaultFreeSwitchPort)
				}
			},
		},
		{
			name:        "freeswitch missing host and password lists both, port is optional",
			env:         map[string]string{EnvTelephonyProvider: "freeswitch"},
			wantCode:    "comms.missing_credentials",
			wantMissing: "ORVEXA_FREESWITCH_HOST, ORVEXA_FREESWITCH_PASSWORD",
		},
		{
			name: "freeswitch explicit port wins over default",
			env: withFakeEnv(map[string]string{
				EnvTelephonyProvider: "freeswitch",
			}),
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.FreeSwitchPort != fakeProviderEnv["ORVEXA_FREESWITCH_PORT"] {
					t.Fatalf("freeswitch port = %q, want explicit env value", c.FreeSwitchPort)
				}
			},
		},
		{
			name: "asterisk port defaults when unset",
			env: map[string]string{
				EnvTelephonyProvider:       "asterisk",
				"ORVEXA_ASTERISK_HOST":     fakeProviderEnv["ORVEXA_ASTERISK_HOST"],
				"ORVEXA_ASTERISK_USERNAME": fakeProviderEnv["ORVEXA_ASTERISK_USERNAME"],
				"ORVEXA_ASTERISK_SECRET":   fakeProviderEnv["ORVEXA_ASTERISK_SECRET"],
			},
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.AsteriskPort != defaultAsteriskPort {
					t.Fatalf("asterisk port = %q, want default %q", c.AsteriskPort, defaultAsteriskPort)
				}
			},
		},
		{
			name: "africastalking voice happy path",
			env: withFakeEnv(map[string]string{
				EnvTelephonyProvider: "africastalking",
			}),
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.ATUsername != Secret(fakeProviderEnv["ORVEXA_AT_USERNAME"]) ||
					c.ATAPIKey != Secret(fakeProviderEnv["ORVEXA_AT_API_KEY"]) ||
					c.ATVoiceProductCode != Secret(fakeProviderEnv["ORVEXA_AT_VOICE_PRODUCT_CODE"]) {
					t.Fatal("africa's talking voice credentials must round-trip")
				}
			},
		},
		{
			name: "env values are trimmed",
			env: map[string]string{
				EnvTelephonyProvider:        "  twilio  ",
				"ORVEXA_TWILIO_ACCOUNT_SID": "  " + fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"] + " ",
				"ORVEXA_TWILIO_AUTH_TOKEN":  fakeProviderEnv["ORVEXA_TWILIO_AUTH_TOKEN"],
				"ORVEXA_TWILIO_FROM_NUMBER": fakeProviderEnv["ORVEXA_TWILIO_FROM_NUMBER"],
			},
			check: func(t *testing.T, c *TelephonyConfig) {
				t.Helper()
				if c.Provider != ProviderTwilio {
					t.Fatalf("provider = %q, want twilio", c.Provider)
				}
				if c.TwilioAccountSID != Secret(fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]) {
					t.Fatal("surrounding whitespace must be trimmed from credential values")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := telephonyFromLookup(lookupFrom(tc.env))
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("parse must succeed: %v", err)
				}
				if err := ValidateTelephony(c); err != nil {
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
			if tc.wantMissing != "" {
				if !strings.Contains(appErr.Message, tc.wantMissing) {
					t.Errorf("message %q must list the missing vars %q", appErr.Message, tc.wantMissing)
				}
				// The message names variables, never values.
				for _, fake := range fakeProviderEnv {
					if strings.Contains(appErr.Message, fake) {
						t.Errorf("error message must not contain credential values")
					}
				}
			}
		})
	}
}

func TestValidateTelephonyHandBuilt(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		err := ValidateTelephony(nil)
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.config_required" {
			t.Fatalf("want comms.config_required, got %v", err)
		}
	})
	t.Run("unknown provider", func(t *testing.T) {
		err := ValidateTelephony(&TelephonyConfig{Provider: "plivo"})
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.unknown_provider" {
			t.Fatalf("want comms.unknown_provider, got %v", err)
		}
	})
	t.Run("empty provider is unknown", func(t *testing.T) {
		err := ValidateTelephony(&TelephonyConfig{})
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.unknown_provider" {
			t.Fatalf("want comms.unknown_provider, got %v", err)
		}
	})
	t.Run("plane mismatch", func(t *testing.T) {
		err := ValidateTelephony(&TelephonyConfig{Provider: ProviderWhatsAppCloud})
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.unsupported_plane" {
			t.Fatalf("want comms.unsupported_plane, got %v", err)
		}
	})
	t.Run("complete config passes", func(t *testing.T) {
		c := &TelephonyConfig{
			Provider:         ProviderTwilio,
			TwilioAccountSID: Secret(fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]),
			TwilioAuthToken:  Secret(fakeProviderEnv["ORVEXA_TWILIO_AUTH_TOKEN"]),
			TwilioFromNumber: Secret(fakeProviderEnv["ORVEXA_TWILIO_FROM_NUMBER"]),
		}
		if err := ValidateTelephony(c); err != nil {
			t.Fatalf("complete config must validate: %v", err)
		}
	})
	t.Run("partial config lists only missing", func(t *testing.T) {
		c := &TelephonyConfig{
			Provider:         ProviderTwilio,
			TwilioAccountSID: Secret(fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]),
		}
		err := ValidateTelephony(c)
		var appErr *apperrors.Error
		if !errors.As(err, &appErr) || appErr.Code != "comms.missing_credentials" {
			t.Fatalf("want comms.missing_credentials, got %v", err)
		}
		if !strings.Contains(appErr.Message, "ORVEXA_TWILIO_AUTH_TOKEN, ORVEXA_TWILIO_FROM_NUMBER") {
			t.Errorf("message must list exactly the missing vars, got %q", appErr.Message)
		}
	})
}

func TestTelephonyCloneIsIndependent(t *testing.T) {
	c := &TelephonyConfig{Provider: ProviderFreeSwitch, FreeSwitchHost: "fs1"}
	cp := c.Clone()
	cp.FreeSwitchHost = "fs2"
	if c.FreeSwitchHost != "fs1" {
		t.Fatal("clone mutation leaked into the original")
	}
	var nilConfig *TelephonyConfig
	if nilConfig.Clone() != nil {
		t.Fatal("nil clone must stay nil")
	}
}
