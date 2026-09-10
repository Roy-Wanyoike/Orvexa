package webhooks

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// loadJSONFixture decodes a testdata file relative to this package. testdata/
// is ignored by the Go toolchain, so the fixtures ship verbatim with the repo.
func loadJSONFixture(t *testing.T, name string, out any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
}

// twilioVector mirrors one vector of testdata/twilio_golden.json.
type twilioVector struct {
	Name                    string `json:"name"`
	AuthToken               string `json:"auth_token"`
	URL                     string `json:"url"`
	FormBody                string `json:"form_body"`
	ExpectedSignatureHeader string `json:"expected_signature_header"`
}

// requireUnauthReject asserts the delivery is rejected with the 401-class
// application error and the expected stable machine code (fail-closed, and
// never a 5xx or a silent accept).
func requireUnauthReject(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected rejection, got nil error (fail-open!)")
	}
	appErr, ok := err.(*apperrors.Error)
	if !ok {
		t.Fatalf("expected *apperrors.Error, got %T: %v", err, err)
	}
	if appErr.Kind != apperrors.KindUnauth {
		t.Fatalf("expected KindUnauth, got %s (%v)", appErr.Kind, appErr)
	}
	if appErr.Code != code {
		t.Fatalf("expected code %q, got %q", code, appErr.Code)
	}
}

// twilioSign is the in-test reference implementation of Twilio's scheme
// (URL + sorted decoded "keyvalue" concatenation). It constructs attack
// scenarios; the GOLDEN fixtures remain the independent cross-implementation
// anchor (Python stdlib, see testdata/generate_golden.py).
func twilioSign(t *testing.T, token, endpoint, rawFormBody string) string {
	t.Helper()
	values, err := url.ParseQuery(rawFormBody)
	if err != nil {
		t.Fatalf("reference signer: %v", err)
	}
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, kv{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	b.WriteString(endpoint)
	for _, p := range pairs {
		b.WriteString(p.k)
		b.WriteString(p.v)
	}
	mac := hmac.New(sha1.New, []byte(token))
	mac.Write([]byte(b.String()))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestTwilioGoldenVectors proves cross-implementation agreement with the
// captured Twilio request-validator spec: the expected signatures were
// produced by testdata/generate_golden.py using Python stdlib hmac/hashlib —
// an implementation fully independent of the Go verifier under test.
func TestTwilioGoldenVectors(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	if len(golden.Vectors) == 0 {
		t.Fatal("no golden vectors")
	}

	for _, v := range golden.Vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			cfg := VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}
			header := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}
			if err := VerifyTwilio(cfg, []byte(v.FormBody), header, v.URL); err != nil {
				t.Fatalf("golden vector %q must validate (Go vs Python cross-implementation): %v", v.Name, err)
			}
		})
	}
}

// TestTwilioMultiTokenRotation pins the documented rotation semantics: Twilio
// keeps two auth tokens live at once and a signature matching ANY configured
// token validates.
func TestTwilioMultiTokenRotation(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)

	var primary, rotated *twilioVector
	for i := range golden.Vectors {
		switch golden.Vectors[i].Name {
		case "basic-multi-param":
			primary = &golden.Vectors[i]
		case "rotated-second-token":
			rotated = &golden.Vectors[i]
		}
	}
	if primary == nil || rotated == nil {
		t.Fatal("golden fixture missing expected vectors")
	}

	header := http.Header{TwilioSignatureHeader: []string{rotated.ExpectedSignatureHeader}}

	// Both tokens configured (rotation window): the rotated-token signature
	// validates.
	both := VerifierConfig{TwilioAuthTokens: []string{primary.AuthToken, rotated.AuthToken}}
	if err := VerifyTwilio(both, []byte(rotated.FormBody), header, rotated.URL); err != nil {
		t.Fatalf("rotated token must validate while both tokens are live: %v", err)
	}

	// Only the rotated token configured: still validates.
	onlyNew := VerifierConfig{TwilioAuthTokens: []string{rotated.AuthToken}}
	if err := VerifyTwilio(onlyNew, []byte(rotated.FormBody), header, rotated.URL); err != nil {
		t.Fatalf("rotated token must validate when configured alone: %v", err)
	}

	// Only the OLD token configured: the rotated signature must reject.
	onlyOld := VerifierConfig{TwilioAuthTokens: []string{primary.AuthToken}}
	requireUnauthReject(t, VerifyTwilio(onlyOld, []byte(rotated.FormBody), header, rotated.URL), twilioCodeInvalidSignature)
}

// TestTwilioWrongTokenRejected: a signature computed with an attacker-chosen
// token is rejected with the 401-class invalid-signature code.
func TestTwilioWrongTokenRejected(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	v := golden.Vectors[0]

	cfg := VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}
	header := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}

	// Honest signature evaluated under an attacker-controlled token list.
	attacker := VerifierConfig{TwilioAuthTokens: []string{"attacker-chosen-token-not-in-list"}}
	requireUnauthReject(t, VerifyTwilio(attacker, []byte(v.FormBody), header, v.URL), twilioCodeInvalidSignature)

	// Honest config, signature computed under a different token.
	evilSig := twilioSign(t, "attacker-chosen-token-not-in-list", v.URL, v.FormBody)
	requireUnauthReject(t, VerifyTwilio(cfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{evilSig}}, v.URL), twilioCodeInvalidSignature)
}

// sentinel reject marker for table-driven cases (never returned by verifiers).
var errSentinelReject = apperrors.Unauth("test.sentinel", "sentinel")

// TestTwilioBodyBindingWhitespaceAndNewlines: the signature covers the exact
// decoded parameter set of the exact delivered body. Whitespace and newline
// mutations of the body change the decoded parameters and must reject. The
// signature HEADER may carry surrounding whitespace (intermediaries fold
// headers); the verifier tolerates exactly that — documented — and nothing
// more.
func TestTwilioBodyBindingWhitespaceAndNewlines(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	v := golden.Vectors[0] // basic-multi-param
	cfg := VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}
	sig := v.ExpectedSignatureHeader

	cases := []struct {
		name string
		body string
		want error // nil = must accept
	}{
		{"control-exact-body", v.FormBody, nil},
		{"trailing-newline-appended", v.FormBody + "\n", errSentinelReject},
		{"leading-newline", "\n" + v.FormBody, errSentinelReject},
		{"crlf-injected", v.FormBody + "\r\n", errSentinelReject},
		{"trailing-space", v.FormBody + " ", errSentinelReject},
		{"interior-value-whitespace-injected", strings.Replace(v.FormBody, "CallStatus=ringing", "CallStatus=ringin g", 1), errSentinelReject},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyTwilio(cfg, []byte(tc.body), http.Header{TwilioSignatureHeader: []string{sig}}, v.URL)
			if tc.want == nil && err != nil {
				t.Fatalf("expected accept, got %v", err)
			}
			if tc.want != nil {
				requireUnauthReject(t, err, twilioCodeInvalidSignature)
			}
		})
	}

	// Header whitespace tolerance (documented): trailing/leading whitespace
	// around an otherwise-valid signature header does not break carriers
	// whose edge folds headers — it cannot weaken security because it does
	// not change the MAC bytes compared.
	t.Run("header-trailing-crlf-tolerated", func(t *testing.T) {
		if err := VerifyTwilio(cfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{sig + "\r\n"}}, v.URL); err != nil {
			t.Fatalf("trailing CRLF on signature header must be tolerated: %v", err)
		}
	})
	t.Run("header-leading-space-tolerated", func(t *testing.T) {
		if err := VerifyTwilio(cfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{" " + sig}}, v.URL); err != nil {
			t.Fatalf("leading space on signature header must be tolerated: %v", err)
		}
	})
	t.Run("header-interior-space-rejected", func(t *testing.T) {
		broken := sig[:len(sig)/2] + " " + sig[len(sig)/2:]
		requireUnauthReject(t, VerifyTwilio(cfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{broken}}, v.URL), twilioCodeInvalidSignature)
	})
}

// TestTwilioURLPitfall pins the documented proxy/URL-reconstruction trap:
// Twilio signs the URL EXACTLY as dialed. The same valid signature must
// validate against the configured external URL and reject against every
// plausible reconstruction the process might observe behind a proxy.
func TestTwilioURLPitfall(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	var v *twilioVector
	for i := range golden.Vectors {
		if golden.Vectors[i].Name == "url-with-query-string" {
			v = &golden.Vectors[i]
		}
	}
	if v == nil {
		t.Fatal("golden fixture missing url-with-query-string vector")
	}
	cfg := VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}
	header := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}
	body := []byte(v.FormBody)

	if err := VerifyTwilio(cfg, body, header, v.URL); err != nil {
		t.Fatalf("exact external URL must validate: %v", err)
	}

	wrongURLs := []string{
		strings.Split(v.URL, "?")[0],                                    // query dropped (proxy strips it)
		strings.Replace(v.URL, "https://", "http://", 1),                // TLS terminated at the edge
		"http://orvexa-internal.svc:8080/v1/webhooks/twilio",            // internal host:port
		strings.Replace(v.URL, "webhook.orvexa.example", "10.0.4.9", 1), // internal IP
		v.URL + "/",        // path normalization
		v.URL + "?extra=1", // extra query param
	}
	for _, u := range wrongURLs {
		err := VerifyTwilio(cfg, body, header, u)
		if err == nil {
			t.Fatalf("URL %q must reject: signature covers the exact dialed URL", u)
		}
		requireUnauthReject(t, err, twilioCodeInvalidSignature)
	}
}

// TestTwilioUTF8ParamSortOrder proves the sort contract: parameters sort by
// UTF-8 bytes, which is identical to Unicode code-point order (a property of
// UTF-8's encoding). The unicode golden vector anchors this against the
// independent Python implementation; this test additionally pins the exact
// expected ordering so a future regression cannot silently reorder.
func TestTwilioUTF8ParamSortOrder(t *testing.T) {
	keys := []string{"a_param", "b_param", "B_param", "emoji", "名前"}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	want := []string{"B_param", "a_param", "b_param", "emoji", "名前"} // 0x42 < 0x61 < 0x62 < 0x65 < U+540D
	for i := range want {
		if sorted[i] != want[i] {
			t.Fatalf("byte-wise sort must equal code-point order: got %q want %q", sorted, want)
		}
	}

	// The multi-byte golden vector must validate under this ordering.
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	for _, v := range golden.Vectors {
		if v.Name != "sorted-params-case-and-unicode" {
			continue
		}
		cfg := VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}
		header := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}
		if err := VerifyTwilio(cfg, []byte(v.FormBody), header, v.URL); err != nil {
			t.Fatalf("unicode sort golden vector must validate: %v", err)
		}
	}
}

// TestTwilioDuplicateKeysValueTieBreak: canonical validators sort (key,value)
// TUPLES, so "Foo=1" sorts before "Foo=2" regardless of wire order — the same
// signature must validate for either body order.
func TestTwilioDuplicateKeysValueTieBreak(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	var v *twilioVector
	for i := range golden.Vectors {
		if golden.Vectors[i].Name == "duplicate-keys-value-tiebreak" {
			v = &golden.Vectors[i]
		}
	}
	if v == nil {
		t.Fatal("golden fixture missing duplicate-keys vector")
	}
	cfg := VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}
	header := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}

	flipped := "Foo=1&Foo=2&Bar=x" // same (k,v) set, opposite wire order
	if err := VerifyTwilio(cfg, []byte(flipped), header, v.URL); err != nil {
		t.Fatalf("signature must be order-independent over the sorted (k,v) set: %v", err)
	}
	// Dropping a parameter changes the signed set: reject.
	requireUnauthReject(t, VerifyTwilio(cfg, []byte("Foo=1&Foo=2"), header, v.URL), twilioCodeInvalidSignature)
}

// TestTwilioNonFormBodyRejected: the verifier implements Twilio's classic
// form-encoded request validator. JSON bodies (Twilio's separate JSON
// webhook mode) must not be ingested under an attacker-controlled signature:
// only a signature whose MAC the verifier itself recomputes from the DECODED
// parameter set validates, and for JSON that recomputation almost never
// reproduces the attacker's raw-concatenation payload.
func TestTwilioNonFormBodyRejected(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	token := golden.Vectors[0].AuthToken
	endpoint := golden.Vectors[0].URL
	cfg := VerifierConfig{TwilioAuthTokens: []string{token}}

	sign := func(key, payload string) string {
		mac := hmac.New(sha1.New, []byte(key))
		mac.Write([]byte(payload))
		return base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}

	// 1. Attacker (no real token) signs url+rawJSON by raw concatenation:
	//    reject regardless of how the body happens to parse.
	jsonBody := `{"event":"call.status","msg":"a=b"}`
	evilSig := sign("attacker-token", endpoint+jsonBody)
	requireUnauthReject(t, VerifyTwilio(cfg, []byte(jsonBody), http.Header{TwilioSignatureHeader: []string{evilSig}}, endpoint), twilioCodeInvalidSignature)

	// 2. Even the REAL token signing the raw JSON concatenation is not the
	//    form scheme: bodies containing '=' decode to (key,value) pairs whose
	//    sorted concatenation differs from the raw bytes → reject.
	realSig := sign(token, endpoint+jsonBody)
	requireUnauthReject(t, VerifyTwilio(cfg, []byte(jsonBody), http.Header{TwilioSignatureHeader: []string{realSig}}, endpoint), twilioCodeInvalidSignature)

	// 3. Malformed form encodings must reject outright (fail-closed):
	//    semicolon separators, invalid percent-escapes.
	for _, body := range []string{"a=b;c=d", "a=%zz"} {
		err := VerifyTwilio(cfg, []byte(body), http.Header{TwilioSignatureHeader: []string{realSig}}, endpoint)
		if err == nil {
			t.Fatalf("malformed form body %q must reject", body)
		}
		requireUnauthReject(t, err, twilioCodeInvalidSignature)
	}
}

// TestTwilioReplayAgainstDifferentBody: a captured (body, signature) pair
// replayed against different parameter sets must reject — the signature is
// bound to the exact decoded parameter multiset and URL.
func TestTwilioReplayAgainstDifferentBody(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	var base *twilioVector
	for i := range golden.Vectors {
		if golden.Vectors[i].Name == "basic-multi-param" {
			base = &golden.Vectors[i]
		}
	}
	if base == nil {
		t.Fatal("golden fixture missing basic-multi-param vector")
	}
	cfg := VerifierConfig{TwilioAuthTokens: []string{base.AuthToken}}
	header := http.Header{TwilioSignatureHeader: []string{base.ExpectedSignatureHeader}}

	replays := []string{
		"CallSid=CAf00dface0123456789abcdef01234567&CallStatus=completed&From=%2B15550001111&To=%2B15550002222",        // status flipped
		"CallSid=CAf00dface0123456789abcdef01234567",                                                                   // params dropped
		"CallSid=CAf00dface0123456789abcdef01234567&CallStatus=ringing&From=%2B15550001111&To=%2B15550002222&Digits=1", // param injected
	}
	for _, body := range replays {
		requireUnauthReject(t, VerifyTwilio(cfg, []byte(body), header, base.URL), twilioCodeInvalidSignature)
	}
	// Replay against a different endpoint URL must reject too.
	requireUnauthReject(t, VerifyTwilio(cfg, []byte(base.FormBody), header, base.URL+"?v=2"), twilioCodeInvalidSignature)
}

// TestTwilioFailClosed covers the acceptance criterion "empty/misconfigured
// secret → fail-closed": no tokens, only-empty tokens, and no external URL
// must all reject even a well-formed signature header.
func TestTwilioFailClosed(t *testing.T) {
	var golden struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &golden)
	v := golden.Vectors[0]
	header := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}
	body := []byte(v.FormBody)

	// No tokens configured.
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{}, body, header, v.URL), twilioCodeMisconfigured)

	// Only empty-string tokens (an empty token is HMAC-able by anyone — it
	// must never become an unsigned ingestion path in disguise).
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{TwilioAuthTokens: []string{""}}, body, header, v.URL), twilioCodeMisconfigured)
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{TwilioAuthTokens: []string{"", ""}}, body, header, v.URL), twilioCodeMisconfigured)

	// Tokens but no external URL: signatures can never be evaluated
	// honestly — reject rather than reconstruct a URL from the request.
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}, body, header, ""), twilioCodeMisconfigured)

	// Missing signature header.
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}, body, http.Header{}, v.URL), twilioCodeInvalidSignature)

	// Non-base64 signature header.
	garbage := http.Header{TwilioSignatureHeader: []string{"not-base64-signature!!"}}
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}, body, garbage, v.URL), twilioCodeInvalidSignature)

	// Base64 but wrong MAC length (SHA-1 is 20 bytes).
	short := http.Header{TwilioSignatureHeader: []string{base64.StdEncoding.EncodeToString(make([]byte, 16))}}
	requireUnauthReject(t, VerifyTwilio(VerifierConfig{TwilioAuthTokens: []string{v.AuthToken}}, body, short, v.URL), twilioCodeInvalidSignature)
}
