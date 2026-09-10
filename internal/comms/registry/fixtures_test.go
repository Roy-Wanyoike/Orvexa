package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

// Clearly-fake credential fixtures. Zero real secret material lives in this
// repository: values are leet-substituted ("t0ken", "s3cret", "v01ce") so a
// redaction assertion cannot pass by coincidence with rendered vocabulary
// such as key names ("auth_token", "app_secret", "voice_product_code").
var fakeProviderEnv = map[string]string{
	"ORVEXA_TWILIO_ACCOUNT_SID":       "ACfakesid0000000000000aa1",
	"ORVEXA_TWILIO_AUTH_TOKEN":        "faket0ken0000000000abcd",
	"ORVEXA_TWILIO_FROM_NUMBER":       "f4kefrom00001111",
	"ORVEXA_WHATSAPP_PHONE_NUMBER_ID": "waf4kepnid000000ee55",
	"ORVEXA_WHATSAPP_ACCESS_TOKEN":    "waf4ket0ken000000ff66",
	"ORVEXA_WHATSAPP_APP_SECRET":      "waf4kes3cret00000gg77",
	"ORVEXA_WHATSAPP_VERIFY_TOKEN":    "waf4kever1fy00000hh88",
	"ORVEXA_AT_USERNAME":              "atf4keuser0000aa11",
	"ORVEXA_AT_API_KEY":               "atf4kekey00000bb22",
	"ORVEXA_AT_VOICE_PRODUCT_CODE":    "atf4kev01ce00000cc33",
	"ORVEXA_AT_SENDER_ID":             "atf4kes3nder000dd44",
	"ORVEXA_FREESWITCH_HOST":          "freeswitch.internal",
	"ORVEXA_FREESWITCH_PORT":          "18021",
	"ORVEXA_FREESWITCH_PASSWORD":      "cluef0urfake0000ii99",
	"ORVEXA_ASTERISK_HOST":            "asterisk.internal",
	"ORVEXA_ASTERISK_PORT":            "15038",
	"ORVEXA_ASTERISK_USERNAME":        "am1adminfake0000jj00",
	"ORVEXA_ASTERISK_SECRET":          "am1s3cret00000kk11",
}

// shortFakeSenderID is deliberately below redactRevealMin bytes to pin the
// full-masking path: no suffix reveal may occur for short credentials.
const shortFakeSenderID = "sh0rt!d"

func lookupFrom(m map[string]string) envGetter {
	return func(key string) string { return m[key] }
}

func withFakeEnv(base map[string]string) map[string]string {
	m := make(map[string]string, len(base)+len(fakeProviderEnv))
	for k, v := range base {
		m[k] = v
	}
	for k, v := range fakeProviderEnv {
		m[k] = v
	}
	return m
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// assertNoCredentialLeak fails when a rendering exposes credential material:
//   - short secrets (< redactRevealMin bytes) must be fully absent — no
//     4-byte window of the value may survive anywhere in the rendering;
//   - long secrets may surface only through their exact "****"+last4 form —
//     no 5-byte window of the value may survive (a 5-byte window can never be
//     part of the allowed last-four reveal).
//
// Failure messages reference indexes and offsets only; fragments of the
// values are never echoed, even into test output.
func assertNoCredentialLeak(t *testing.T, rendering string, secrets []string) {
	t.Helper()
	for idx, s := range secrets {
		if s == "" {
			continue
		}
		if len(s) < redactRevealMin {
			for i := 0; i+4 <= len(s); i++ {
				if strings.Contains(rendering, s[i:i+4]) {
					t.Errorf("rendering leaks 4-byte window at offset %d of short secret #%d (full-masking contract violated)", i, idx)
				}
			}
			continue
		}
		if !strings.Contains(rendering, "****"+s[len(s)-4:]) {
			t.Errorf("rendering missing masked last-four form for long secret #%d", idx)
		}
		for i := 0; i+5 <= len(s); i++ {
			if strings.Contains(rendering, s[i:i+5]) {
				t.Errorf("rendering leaks 5-byte window at offset %d of long secret #%d", i, idx)
			}
		}
	}
}
