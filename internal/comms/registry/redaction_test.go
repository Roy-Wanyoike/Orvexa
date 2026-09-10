package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// telephonyRenderings exercises every rendering path that could leak: the
// Stringer, the fmt verbs covered by Stringer/GoStringer (v, +v, s, q, #v —
// including both pointer and value forms), encoding/json, and slog JSON/text
// handlers (which resolve LogValue).
func telephonyRenderings(t *testing.T, c *TelephonyConfig) map[string]string {
	t.Helper()
	out := map[string]string{
		"String":        c.String(),
		"fmtVptr":       fmt.Sprintf("%v", c),
		"fmtVvalue":     fmt.Sprintf("%v", *c),
		"fmtPlusVvalue": fmt.Sprintf("%+v", *c),
		"fmtS":          fmt.Sprintf("%s", c),
		"fmtQ":          fmt.Sprintf("%q", c),
		"fmtGoString":   fmt.Sprintf("%#v", c),
		"jsonPtr":       string(mustJSON(t, c)),
		"jsonValue":     string(mustJSON(t, *c)),
	}
	var jsonBuf, textBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Info("provider_config", "config", c)
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("provider_config", "config", c)
	out["slogJSON"] = jsonBuf.String()
	out["slogText"] = textBuf.String()
	return out
}

func messagingRenderings(t *testing.T, c *MessagingConfig) map[string]string {
	t.Helper()
	out := map[string]string{
		"String":        c.String(),
		"fmtVptr":       fmt.Sprintf("%v", c),
		"fmtVvalue":     fmt.Sprintf("%v", *c),
		"fmtPlusVvalue": fmt.Sprintf("%+v", *c),
		"fmtS":          fmt.Sprintf("%s", c),
		"fmtQ":          fmt.Sprintf("%q", c),
		"fmtGoString":   fmt.Sprintf("%#v", c),
		"jsonPtr":       string(mustJSON(t, c)),
		"jsonValue":     string(mustJSON(t, *c)),
	}
	var jsonBuf, textBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonBuf, nil)).Info("provider_config", "config", c)
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("provider_config", "config", c)
	out["slogJSON"] = jsonBuf.String()
	out["slogText"] = textBuf.String()
	return out
}

func telephonySecretValues(t *testing.T, c *TelephonyConfig) []string {
	t.Helper()
	spec, ok := Lookup(c.Provider)
	if !ok {
		t.Fatalf("provider %q must be registered", c.Provider)
	}
	targets := c.envTargets()
	var vals []string
	for _, fs := range spec.Telephony {
		if fs.Secret {
			vals = append(vals, string(*targets[fs.Env]))
		}
	}
	return vals
}

func messagingSecretValues(t *testing.T, c *MessagingConfig) []string {
	t.Helper()
	spec, ok := Lookup(c.Provider)
	if !ok {
		t.Fatalf("provider %q must be registered", c.Provider)
	}
	targets := c.envTargets()
	var vals []string
	for _, fs := range spec.Messaging {
		if fs.Secret {
			vals = append(vals, string(*targets[fs.Env]))
		}
	}
	return vals
}

// TestTelephonyRenderingLeaksNoCredentials is the adversarial redaction
// matrix: every voice provider × every rendering path, checked for credential
// fragments with plain and sub-threshold values.
func TestTelephonyRenderingLeaksNoCredentials(t *testing.T) {
	for _, p := range []ProviderName{
		ProviderSimulator, ProviderTwilio, ProviderAfricasTalking, ProviderFreeSwitch, ProviderAsterisk,
	} {
		t.Run(string(p), func(t *testing.T) {
			env := withFakeEnv(map[string]string{EnvTelephonyProvider: string(p)})
			c, err := telephonyFromLookup(lookupFrom(env))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			secrets := telephonySecretValues(t, c)
			renderings := telephonyRenderings(t, c)
			for name, rendering := range renderings {
				assertNoCredentialLeak(t, rendering, secrets)
			}

			// Endpoint addressing (non-credential) must stay legible for operators.
			if p == ProviderFreeSwitch {
				out := renderings["String"]
				if !strings.Contains(out, "freeswitch.host=freeswitch.internal") ||
					!strings.Contains(out, "freeswitch.port="+fakeProviderEnv["ORVEXA_FREESWITCH_PORT"]) {
					t.Errorf("plain endpoint fields must render raw, got %q", out)
				}
			}
			if p == ProviderAsterisk {
				out := renderings["String"]
				if !strings.Contains(out, "asterisk.host=asterisk.internal") ||
					!strings.Contains(out, "asterisk.port="+fakeProviderEnv["ORVEXA_ASTERISK_PORT"]) {
					t.Errorf("plain endpoint fields must render raw, got %q", out)
				}
			}
			if p == ProviderSimulator && renderings["String"] != "telephony(provider=simulator)" {
				t.Errorf("simulator rendering = %q, want exact stable form", renderings["String"])
			}
		})
	}
}

// TestMessagingRenderingLeaksNoCredentials is the messaging twin and adds the
// sub-threshold (short credential) case to pin the full-masking path.
func TestMessagingRenderingLeaksNoCredentials(t *testing.T) {
	for _, p := range []ProviderName{
		ProviderSimulator, ProviderTwilio, ProviderWhatsAppCloud, ProviderAfricasTalking,
	} {
		t.Run(string(p), func(t *testing.T) {
			env := withFakeEnv(map[string]string{EnvMessagingProvider: string(p)})
			if p == ProviderAfricasTalking {
				env["ORVEXA_AT_SENDER_ID"] = shortFakeSenderID
			}
			c, err := messagingFromLookup(lookupFrom(env))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			secrets := messagingSecretValues(t, c)
			renderings := messagingRenderings(t, c)
			for name, rendering := range renderings {
				assertNoCredentialLeak(t, rendering, secrets)
			}
			if p == ProviderAfricasTalking {
				out := renderings["String"]
				if !strings.Contains(out, "at.sender_id=****") {
					t.Errorf("short credential must render fully masked, got %q", out)
				}
				if strings.Contains(out, "at.sender_id=****"+shortFakeSenderID[len(shortFakeSenderID)-4:]) {
					t.Errorf("short credential must not reveal a last-four suffix, got %q", out)
				}
			}
			if p == ProviderSimulator && renderings["String"] != "messaging(provider=simulator)" {
				t.Errorf("simulator rendering = %q, want exact stable form", renderings["String"])
			}
		})
	}
}

// TestRenderedJSONStructure pins the JSON surface: redacted, keyed, and
// parseable — no raw field names carrying raw values.
func TestRenderedJSONStructure(t *testing.T) {
	env := withFakeEnv(map[string]string{EnvTelephonyProvider: "twilio"})
	c, err := telephonyFromLookup(lookupFrom(env))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(mustJSON(t, c), &m); err != nil {
		t.Fatalf("rendered JSON must unmarshal: %v", err)
	}
	if m["provider"] != string(ProviderTwilio) {
		t.Errorf("provider key = %q, want twilio", m["provider"])
	}
	if _, ok := m["twilio.auth_token"]; !ok {
		t.Errorf("rendered JSON must carry the masked auth_token key")
	}
	assertNoCredentialLeak(t, mustJSON(t, c), telephonySecretValues(t, c))
}

// TestErrorStringsCarryNoCredentialMaterial: validation failures embed env
// var names only; hand-built configs passed through Validate produce the same
// name-only diagnostics even when the fields hold values.
func TestErrorStringsCarryNoCredentialMaterial(t *testing.T) {
	c := &TelephonyConfig{
		Provider:         ProviderTwilio,
		TwilioAccountSID: Secret(fakeProviderEnv["ORVEXA_TWILIO_ACCOUNT_SID"]),
	}
	err := ValidateTelephony(c)
	if err == nil {
		t.Fatal("partial config must fail validation")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ORVEXA_TWILIO_AUTH_TOKEN") {
		t.Errorf("message must name the missing var, got %q", msg)
	}
	for _, fake := range fakeProviderEnv {
		if strings.Contains(msg, fake) {
			t.Fatal("error message must never contain credential values")
		}
	}
}
