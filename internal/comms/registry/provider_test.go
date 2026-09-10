package registry

import (
	"errors"
	"strings"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func TestValidateKnownProviders(t *testing.T) {
	cases := []struct {
		name     ProviderName
		wantCode string // "" = must succeed
	}{
		{ProviderSimulator, ""},
		{ProviderTwilio, ""},
		{ProviderWhatsAppCloud, ""},
		{ProviderAfricasTalking, ""},
		{ProviderFreeSwitch, ""},
		{ProviderAsterisk, ""},
		{"", "comms.unknown_provider"},
		{"Twilio", "comms.unknown_provider"},  // exact-match contract: no case folding
		{"twilio ", "comms.unknown_provider"}, // exact-match contract: no implicit trimming
		{"plivo", "comms.unknown_provider"},
	}
	for _, tc := range cases {
		t.Run(`name="`+string(tc.name)+`"`, func(t *testing.T) {
			err := Validate(tc.name)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("Validate(%q) must succeed, got %v", tc.name, err)
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
			if !strings.Contains(appErr.Message, string(ProviderSimulator)) ||
				!strings.Contains(appErr.Message, string(ProviderAsterisk)) {
				t.Errorf("unknown-provider message must list registered providers, got %q", appErr.Message)
			}
		})
	}
}

func TestValidateForPlane(t *testing.T) {
	cases := []struct {
		name     ProviderName
		plane    Plane
		wantCode string // "" = must succeed
	}{
		{ProviderSimulator, PlaneVoice, ""},
		{ProviderSimulator, PlaneMessaging, ""},
		{ProviderTwilio, PlaneVoice, ""},
		{ProviderTwilio, PlaneMessaging, ""},
		{ProviderWhatsAppCloud, PlaneMessaging, ""},
		{ProviderWhatsAppCloud, PlaneVoice, "comms.unsupported_plane"},
		{ProviderAfricasTalking, PlaneVoice, ""},
		{ProviderAfricasTalking, PlaneMessaging, ""},
		{ProviderFreeSwitch, PlaneVoice, ""},
		{ProviderFreeSwitch, PlaneMessaging, "comms.unsupported_plane"},
		{ProviderAsterisk, PlaneVoice, ""},
		{ProviderAsterisk, PlaneMessaging, "comms.unsupported_plane"},
		{"plivo", PlaneVoice, "comms.unknown_provider"},
	}
	for _, tc := range cases {
		t.Run(string(tc.name)+"/"+tc.plane.String(), func(t *testing.T) {
			err := ValidateForPlane(tc.name, tc.plane)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateForPlane(%q, %s) must succeed, got %v", tc.name, tc.plane, err)
				}
				return
			}
			var appErr *apperrors.Error
			if !errors.As(err, &appErr) {
				t.Fatalf("want *apperrors.Error, got %v", err)
			}
			if appErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", appErr.Code, tc.wantCode)
			}
			if tc.wantCode == "comms.unsupported_plane" && !strings.Contains(appErr.Message, "supports:") {
				t.Errorf("plane-mismatch message must state the supported planes, got %q", appErr.Message)
			}
		})
	}
}

func TestNamesCompleteAndSorted(t *testing.T) {
	want := []ProviderName{
		ProviderAfricasTalking,
		ProviderAsterisk,
		ProviderFreeSwitch,
		ProviderSimulator,
		ProviderTwilio,
		ProviderWhatsAppCloud,
	}
	got := Names()
	if len(got) != len(want) {
		t.Fatalf("Names() length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProviderSpecPlanes(t *testing.T) {
	cases := []struct {
		name ProviderName
		want Plane
	}{
		{ProviderSimulator, PlaneVoice | PlaneMessaging},
		{ProviderTwilio, PlaneVoice | PlaneMessaging},
		{ProviderWhatsAppCloud, PlaneMessaging},
		{ProviderAfricasTalking, PlaneVoice | PlaneMessaging},
		{ProviderFreeSwitch, PlaneVoice},
		{ProviderAsterisk, PlaneVoice},
	}
	for _, tc := range cases {
		spec, ok := Lookup(tc.name)
		if !ok {
			t.Fatalf("provider %q must be registered", tc.name)
		}
		if spec.Planes != tc.want {
			t.Errorf("provider %q planes = %s, want %s", tc.name, spec.Planes, tc.want)
		}
		if spec.Summary == "" {
			t.Errorf("provider %q must carry a summary", tc.name)
		}
	}
}

// TestTelephonySpecTargetCoverage pins the registry table to the struct:
// every spec env var must have a struct target, every target must be
// declared, and no env var may appear twice within a plane.
func TestTelephonySpecTargetCoverage(t *testing.T) {
	targets := (&TelephonyConfig{}).envTargets()
	seen := map[string]bool{}
	for _, name := range Names() {
		spec, _ := Lookup(name)
		for _, fs := range spec.Telephony {
			if seen[fs.Env] {
				t.Fatalf("env %s declared twice across telephony specs", fs.Env)
			}
			seen[fs.Env] = true
			if _, ok := targets[fs.Env]; !ok {
				t.Fatalf("telephony spec env %s has no struct target", fs.Env)
			}
		}
	}
	for env := range targets {
		if !seen[env] {
			t.Fatalf("telephony env target %s is not declared in any provider spec", env)
		}
	}
}

// TestMessagingSpecTargetCoverage is the messaging-plane twin.
func TestMessagingSpecTargetCoverage(t *testing.T) {
	targets := (&MessagingConfig{}).envTargets()
	seen := map[string]bool{}
	for _, name := range Names() {
		spec, _ := Lookup(name)
		for _, fs := range spec.Messaging {
			if seen[fs.Env] {
				t.Fatalf("env %s declared twice across messaging specs", fs.Env)
			}
			seen[fs.Env] = true
			if _, ok := targets[fs.Env]; !ok {
				t.Fatalf("messaging spec env %s has no struct target", fs.Env)
			}
		}
	}
	for env := range targets {
		if !seen[env] {
			t.Fatalf("messaging env target %s is not declared in any provider spec", env)
		}
	}
}

func TestSpecEnvVarsWellFormed(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		all := append(append([]FieldSpec{}, spec.Telephony...), spec.Messaging...)
		for _, fs := range all {
			if !strings.HasPrefix(fs.Env, "ORVEXA_") {
				t.Errorf("provider %q env %q must carry the ORVEXA_ prefix", name, fs.Env)
			}
			if strings.TrimSpace(fs.Env) != fs.Env || fs.Env != strings.ToUpper(fs.Env) {
				t.Errorf("provider %q env %q must be clean uppercase", name, fs.Env)
			}
		}
	}
}

// TestSecretClassificationMatchesContract pins which env vars are credential
// material (redacted everywhere) versus plain endpoint addressing (rendered
// raw). A drift here is a security-relevant regression.
func TestSecretClassificationMatchesContract(t *testing.T) {
	cases := []struct {
		provider   ProviderName
		plane      Plane
		secretEnvs []string
		plainEnvs  []string
	}{
		{ProviderSimulator, PlaneVoice, nil, nil},
		{ProviderSimulator, PlaneMessaging, nil, nil},
		{ProviderTwilio, PlaneVoice,
			[]string{"ORVEXA_TWILIO_ACCOUNT_SID", "ORVEXA_TWILIO_AUTH_TOKEN", "ORVEXA_TWILIO_FROM_NUMBER"}, nil},
		{ProviderTwilio, PlaneMessaging,
			[]string{"ORVEXA_TWILIO_ACCOUNT_SID", "ORVEXA_TWILIO_AUTH_TOKEN", "ORVEXA_TWILIO_FROM_NUMBER"}, nil},
		{ProviderWhatsAppCloud, PlaneMessaging,
			[]string{"ORVEXA_WHATSAPP_PHONE_NUMBER_ID", "ORVEXA_WHATSAPP_ACCESS_TOKEN", "ORVEXA_WHATSAPP_APP_SECRET", "ORVEXA_WHATSAPP_VERIFY_TOKEN"}, nil},
		{ProviderAfricasTalking, PlaneVoice,
			[]string{"ORVEXA_AT_USERNAME", "ORVEXA_AT_API_KEY", "ORVEXA_AT_VOICE_PRODUCT_CODE"}, nil},
		{ProviderAfricasTalking, PlaneMessaging,
			[]string{"ORVEXA_AT_USERNAME", "ORVEXA_AT_API_KEY", "ORVEXA_AT_SENDER_ID"}, nil},
		{ProviderFreeSwitch, PlaneVoice,
			[]string{"ORVEXA_FREESWITCH_PASSWORD"},
			[]string{"ORVEXA_FREESWITCH_HOST", "ORVEXA_FREESWITCH_PORT"}},
		{ProviderAsterisk, PlaneVoice,
			[]string{"ORVEXA_ASTERISK_USERNAME", "ORVEXA_ASTERISK_SECRET"},
			[]string{"ORVEXA_ASTERISK_HOST", "ORVEXA_ASTERISK_PORT"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider)+"/"+tc.plane.String(), func(t *testing.T) {
			spec, ok := Lookup(tc.provider)
			if !ok {
				t.Fatalf("provider %q must be registered", tc.provider)
			}
			fields := spec.Telephony
			if tc.plane == PlaneMessaging {
				fields = spec.Messaging
			}
			var gotSecret, gotPlain []string
			for _, fs := range fields {
				if fs.Secret {
					gotSecret = append(gotSecret, fs.Env)
				} else {
					gotPlain = append(gotPlain, fs.Env)
				}
			}
			assertStringSetEqual(t, "secret envs", gotSecret, tc.secretEnvs)
			assertStringSetEqual(t, "plain envs", gotPlain, tc.plainEnvs)
		})
	}
}

func assertStringSetEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	seen := map[string]int{}
	for _, s := range want {
		seen[s]++
	}
	for _, s := range got {
		seen[s]--
		if seen[s] < 0 {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// TestEachSpecEnvFeedsDistinctField parses a config with every spec env var
// set to a value derived from the variable's own name and asserts each one
// landed in its own, non-empty struct field — i.e. the parser and the
// registry table cannot drift apart or alias one field silently.
func TestEachSpecEnvFeedsDistinctField(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)

		if len(spec.Telephony) > 0 {
			t.Run(string(name)+"/telephony", func(t *testing.T) {
				env := map[string]string{EnvTelephonyProvider: string(name)}
				for _, fs := range spec.Telephony {
					env[fs.Env] = "fake-" + strings.ToLower(strings.TrimPrefix(fs.Env, "ORVEXA_"))
				}
				c, err := telephonyFromLookup(lookupFrom(env))
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				assertAllTargetsFed(t, c.envTargets(), spec.Telephony)
			})
		}
		if len(spec.Messaging) > 0 {
			t.Run(string(name)+"/messaging", func(t *testing.T) {
				env := map[string]string{EnvMessagingProvider: string(name)}
				for _, fs := range spec.Messaging {
					env[fs.Env] = "fake-" + strings.ToLower(strings.TrimPrefix(fs.Env, "ORVEXA_"))
				}
				c, err := messagingFromLookup(lookupFrom(env))
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				assertAllTargetsFed(t, c.envTargets(), spec.Messaging)
			})
		}
	}
}

func assertAllTargetsFed(t *testing.T, targets map[string]fieldRef, fields []FieldSpec) {
	t.Helper()
	for _, fs := range fields {
		ref, ok := targets[fs.Env]
		if !ok {
			t.Fatalf("env %s has no target", fs.Env)
		}
		want := "fake-" + strings.ToLower(strings.TrimPrefix(fs.Env, "ORVEXA_"))
		if ref.get() != want {
			t.Fatalf("env %s did not land in its own field (aliased or dropped write)", fs.Env)
		}
	}
}
