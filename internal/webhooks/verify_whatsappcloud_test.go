package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// waVector mirrors one vector of testdata/whatsappcloud_golden.json.
type waVector struct {
	Name                    string `json:"name"`
	AppSecret               string `json:"app_secret"`
	RawBody                 string `json:"raw_body"`
	ExpectedSignatureHeader string `json:"expected_signature_header"`
}

func loadWAGolden(t *testing.T) []waVector {
	t.Helper()
	var golden struct {
		Vectors []waVector `json:"vectors"`
	}
	loadJSONFixture(t, "whatsappcloud_golden.json", &golden)
	if len(golden.Vectors) == 0 {
		t.Fatal("no golden vectors")
	}
	return golden.Vectors
}

// waSign computes the scheme in-test for attack scenarios; the GOLDEN
// fixtures remain the independent cross-implementation anchor (Python stdlib,
// see testdata/generate_golden.py).
func waSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return whatsappSigPrefix + hex.EncodeToString(mac.Sum(nil))
}

func waHeader(sig string) http.Header {
	return http.Header{WhatsAppSignatureHeader: []string{sig}}
}

// TestWhatsAppGoldenVectors proves cross-implementation agreement with the
// captured Meta scheme: expected signatures were produced by
// testdata/generate_golden.py (Python stdlib hmac/hashlib), independent of
// the Go verifier.
func TestWhatsAppGoldenVectors(t *testing.T) {
	for _, v := range loadWAGolden(t) {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			cfg := VerifierConfig{WhatsAppAppSecret: v.AppSecret}
			err := VerifyWhatsAppCloud(cfg, []byte(v.RawBody), waHeader(v.ExpectedSignatureHeader), "")
			if err != nil {
				t.Fatalf("golden vector %q must validate (Go vs Python cross-implementation): %v", v.Name, err)
			}
		})
	}
}

// TestWhatsAppBodyByteBinding: Meta signs the EXACT raw body bytes — a
// one-byte mutation (whitespace, newline, field) must reject.
func TestWhatsAppBodyByteBinding(t *testing.T) {
	vectors := loadWAGolden(t)
	var signed, unsigned waVector
	for _, v := range vectors {
		switch v.Name {
		case "simple-json":
			signed = v
		case "trailing-newline-body":
			unsigned = v
		}
	}
	cfg := VerifierConfig{WhatsAppAppSecret: signed.AppSecret}

	cases := []struct {
		name string
		body string
		want error
	}{
		{"control-exact-bytes", signed.RawBody, nil},
		{"trailing-newline", signed.RawBody + "\n", errSentinelReject},
		{"trailing-space", signed.RawBody + " ", errSentinelReject},
		{"leading-newline", "\n" + signed.RawBody, errSentinelReject},
		{"crlf-injected", signed.RawBody + "\r\n", errSentinelReject},
		// Golden vector signed WITH its trailing newline; delivering the same
		// JSON WITHOUT the newline must reject (byte-level binding).
		{"signed-newline-delivered-without", strings.TrimSuffix(unsigned.RawBody, "\n"), errSentinelReject},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyWhatsAppCloud(cfg, []byte(tc.body), waHeader(signed.ExpectedSignatureHeader), "")
			if tc.want == nil && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
			if tc.want != nil {
				requireUnauthReject(t, err, waCodeInvalidSignature)
			}
		})
	}
}

// TestWhatsAppConstantTimeCompareProperties demonstrates the documented
// comparison properties that make the check timing-safe:
//
//   - the provided digest is hex-DECODED to raw MAC bytes BEFORE comparison
//     (uppercase/mixed-case encodings of the same MAC validate — a hex-STRING
//     compare would reject them);
//   - the comparison itself is crypto/hmac's hmac.Equal over the raw bytes.
//
// True timing invariance is a property of hmac.Equal and cannot be measured
// reliably in CI; these tests pin the observable behaviors the doc comment
// promises (case-normalization via decode, position-independent rejection).
func TestWhatsAppConstantTimeCompareProperties(t *testing.T) {
	vectors := loadWAGolden(t)
	v := vectors[0] // simple-json
	cfg := VerifierConfig{WhatsAppAppSecret: v.AppSecret}
	body := []byte(v.RawBody)

	// Uppercase and mixed-case encodings of the SAME MAC validate.
	upper := whatsappSigPrefix + strings.ToUpper(strings.TrimPrefix(v.ExpectedSignatureHeader, whatsappSigPrefix))
	if err := VerifyWhatsAppCloud(cfg, body, waHeader(upper), ""); err != nil {
		t.Fatalf("uppercase encoding of the same MAC must validate (raw-byte compare, not hex-string): %v", err)
	}
	mixed := whatsappSigPrefix
	for i, r := range strings.TrimPrefix(v.ExpectedSignatureHeader, whatsappSigPrefix) {
		if i%2 == 0 {
			mixed += strings.ToUpper(string(r))
		} else {
			mixed += string(r)
		}
	}
	if err := VerifyWhatsAppCloud(cfg, body, waHeader(mixed), ""); err != nil {
		t.Fatalf("mixed-case encoding of the same MAC must validate: %v", err)
	}

	// Corruption at the first and last byte lands in the same rejection
	// class: the compare does not leak where the bytes diverge.
	digest, err := hex.DecodeString(strings.TrimPrefix(v.ExpectedSignatureHeader, whatsappSigPrefix))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := func(i int) string {
		d := append([]byte(nil), digest...)
		d[i] ^= 0x01
		return whatsappSigPrefix + hex.EncodeToString(d)
	}
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader(corrupt(0)), ""), waCodeInvalidSignature)
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader(corrupt(len(digest)-1)), ""), waCodeInvalidSignature)

	// Truncated digest: wrong MAC length must reject before comparison.
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body,
		waHeader(v.ExpectedSignatureHeader[:len(v.ExpectedSignatureHeader)-8]), ""), waCodeInvalidSignature)
}

// TestWhatsAppFailClosed: empty/missing app secret, missing/garbage header —
// every unprovable delivery rejects with a 401-class error.
func TestWhatsAppFailClosed(t *testing.T) {
	vectors := loadWAGolden(t)
	v := vectors[0]

	// Empty app secret: HMAC with an empty key is computable by anyone — it
	// must never become an unsigned ingestion path in disguise.
	requireUnauthReject(t, VerifyWhatsAppCloud(VerifierConfig{}, []byte(v.RawBody), waHeader(v.ExpectedSignatureHeader), ""), waCodeMisconfigured)

	cfg := VerifierConfig{WhatsAppAppSecret: v.AppSecret}
	body := []byte(v.RawBody)

	// Missing header.
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, http.Header{}, ""), waCodeInvalidSignature)

	// Bare scheme marker / empty digest.
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader("sha256"), ""), waCodeInvalidSignature)
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader("sha256="), ""), waCodeInvalidSignature)

	// Wrong scheme marker (legacy sha1 pasted into the 256 header).
	sha1AsHex := strings.Repeat("ab", 32)
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader("sha1="+sha1AsHex), ""), waCodeInvalidSignature)

	// Non-hex digest / odd-length digest.
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader("sha256="+strings.Repeat("zz", 32)), ""), waCodeInvalidSignature)
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader("sha256="+strings.Repeat("ab", 31)+"a"), ""), waCodeInvalidSignature)

	// Whitespace inside the header: strict — Meta delivers exact bytes, and
	// a lenient parse here could mask a mangled digest.
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, waHeader("sha256= "+strings.TrimPrefix(v.ExpectedSignatureHeader, whatsappSigPrefix)), ""), waCodeInvalidSignature)
}

// TestWhatsAppWrongSecretRejected: signature computed under an
// attacker-chosen secret must reject.
func TestWhatsAppWrongSecretRejected(t *testing.T) {
	vectors := loadWAGolden(t)
	v := vectors[0]
	cfg := VerifierConfig{WhatsAppAppSecret: v.AppSecret}
	evilSig := waSign("attacker-chosen-secret", []byte(v.RawBody))
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, []byte(v.RawBody), waHeader(evilSig), ""), waCodeInvalidSignature)
}

// TestWhatsAppLegacySHA1HeaderIgnored: Meta also transmits a legacy
// X-Hub-Signature-1 on some surfaces. It must be IGNORED — never accepted as
// a fallback — and only X-Hub-Signature-256 decides.
func TestWhatsAppLegacySHA1HeaderIgnored(t *testing.T) {
	vectors := loadWAGolden(t)
	v := vectors[0]
	cfg := VerifierConfig{WhatsAppAppSecret: v.AppSecret}
	body := []byte(v.RawBody)

	legacyOnly := http.Header{
		"X-Hub-Signature-1": []string{"sha1=0123456789abcdef0123456789abcdef01234567"},
	}
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, body, legacyOnly, ""), waCodeInvalidSignature)

	// Legacy header present alongside a valid 256: the 256 header decides.
	both := http.Header{
		"X-Hub-Signature-1":     legacyOnly["X-Hub-Signature-1"],
		WhatsAppSignatureHeader: []string{v.ExpectedSignatureHeader},
	}
	if err := VerifyWhatsAppCloud(cfg, body, both, ""); err != nil {
		t.Fatalf("valid sha256 header must decide regardless of legacy header: %v", err)
	}
}

// TestWhatsAppReplayAcrossBodies: a captured (body, signature) pair replayed
// against different bodies must reject.
func TestWhatsAppReplayAcrossBodies(t *testing.T) {
	vectors := loadWAGolden(t)
	var a, b waVector
	for _, v := range vectors {
		switch v.Name {
		case "simple-json":
			a = v
		case "unicode-body":
			b = v
		}
	}
	cfg := VerifierConfig{WhatsAppAppSecret: a.AppSecret}
	requireUnauthReject(t, VerifyWhatsAppCloud(cfg, []byte(b.RawBody), waHeader(a.ExpectedSignatureHeader), ""), waCodeInvalidSignature)
}
