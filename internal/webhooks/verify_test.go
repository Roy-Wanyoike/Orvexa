package webhooks

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ─────────────────────────────────────────────────────────────────────────────
// Attack matrix (issue #24): every case in testdata/attack_matrix.json is
// really executed against the Go verifiers below. The matrix is the
// adversarial counterpart to the golden vectors: where the goldens prove the
// happy path agrees with an independent implementation (Python), the matrix
// proves the unhappy paths fail CLOSED.
// ─────────────────────────────────────────────────────────────────────────────

type attackCase struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Attack   string `json:"attack"`
	Expect   string `json:"expect"`
}

// attackMatrixFixture loads the shared synthetic credentials and scenario
// inputs used to construct each attack case.
type attackMatrixFixture struct {
	twVectors map[string]twilioVector
	twTokens  []string
	waVectors map[string]waVector
	waSecret  string
}

func loadAttackFixture(t *testing.T) *attackMatrixFixture {
	t.Helper()
	f := &attackMatrixFixture{
		twVectors: map[string]twilioVector{},
		waVectors: map[string]waVector{},
	}

	var tw struct {
		Vectors []twilioVector `json:"vectors"`
	}
	loadJSONFixture(t, "twilio_golden.json", &tw)
	twTokenSet := map[string]bool{}
	for _, v := range tw.Vectors {
		f.twVectors[v.Name] = v
		twTokenSet[v.AuthToken] = true
	}
	for tok := range twTokenSet {
		f.twTokens = append(f.twTokens, tok)
	}

	var wa struct {
		Vectors []waVector `json:"vectors"`
	}
	loadJSONFixture(t, "whatsappcloud_golden.json", &wa)
	for _, v := range wa.Vectors {
		f.waVectors[v.Name] = v
	}
	f.waSecret = wa.Vectors[0].AppSecret
	return f
}

// scenario builds the (cfg, body, header, externalURL) tuple for one attack
// case id.
func (f *attackMatrixFixture) scenario(t *testing.T, id string) (VerifierConfig, []byte, http.Header, string) {
	t.Helper()
	const attackerToken = "attacker-chosen-token-not-in-the-list"
	const attackerSecret = "attacker-chosen-app-secret"

	twCfg := VerifierConfig{TwilioAuthTokens: f.twTokens, ExternalURL: f.twVectors["basic-multi-param"].URL}
	waCfg := VerifierConfig{WhatsAppAppSecret: f.waSecret}
	atCfg := VerifierConfig{ATAllowedCIDRs: []string{"10.0.0.0/8"}}

	flip := func(sig string, i int) string {
		raw, err := hex.DecodeString(strings.TrimPrefix(sig, whatsappSigPrefix))
		if err != nil {
			t.Fatal(err)
		}
		raw[i] ^= 0x01
		return whatsappSigPrefix + hex.EncodeToString(raw)
	}

	switch id {
	// ── Twilio ───────────────────────────────────────────────────────────
	case "tw-valid":
		v := f.twVectors["basic-multi-param"]
		return twCfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}, v.URL
	case "tw-wrong-token":
		v := f.twVectors["basic-multi-param"]
		sig := twilioSign(t, attackerToken, v.URL, v.FormBody)
		return twCfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{sig}}, v.URL
	case "tw-body-substituted":
		v := f.twVectors["basic-multi-param"]
		other := f.twVectors["plus-and-percent-encoding"]
		return twCfg, []byte(other.FormBody), http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}, v.URL
	case "tw-replay-different-body":
		v := f.twVectors["basic-multi-param"]
		other := f.twVectors["rotated-second-token"]
		return twCfg, []byte(other.FormBody), http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}, v.URL
	case "tw-missing-header":
		v := f.twVectors["basic-multi-param"]
		return twCfg, []byte(v.FormBody), http.Header{}, v.URL
	case "tw-empty-token-list":
		v := f.twVectors["basic-multi-param"]
		cfg := VerifierConfig{ExternalURL: v.URL} // no tokens
		return cfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}, v.URL
	case "tw-bad-base64":
		v := f.twVectors["basic-multi-param"]
		return twCfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{"%%not-base64%%"}}, v.URL
	case "tw-trailing-newline":
		v := f.twVectors["basic-multi-param"]
		return twCfg, []byte(v.FormBody + "\n"), http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}, v.URL
	case "tw-internal-url":
		v := f.twVectors["basic-multi-param"]
		return twCfg, []byte(v.FormBody), http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}},
			"http://orvexa-internal.svc:8080/v1/webhooks/twilio"
	case "tw-json-body-not-form-signed":
		body := `{"event":"call.status","msg":"a=b"}`
		sig := twilioSign(t, attackerToken, f.twVectors["basic-multi-param"].URL, body)
		return twCfg, []byte(body), http.Header{TwilioSignatureHeader: []string{sig}}, f.twVectors["basic-multi-param"].URL

	// ── WhatsApp Cloud ───────────────────────────────────────────────────
	case "wa-valid":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader(v.ExpectedSignatureHeader), ""
	case "wa-wrong-secret":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader(waSign(attackerSecret, []byte(v.RawBody))), ""
	case "wa-body-substituted":
		v := f.waVectors["simple-json"]
		other := f.waVectors["unicode-body"]
		return waCfg, []byte(other.RawBody), waHeader(v.ExpectedSignatureHeader), ""
	case "wa-replay-different-body":
		v := f.waVectors["simple-json"]
		other := f.waVectors["trailing-newline-body"]
		return waCfg, []byte(other.RawBody), waHeader(v.ExpectedSignatureHeader), ""
	case "wa-missing-header":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), http.Header{}, ""
	case "wa-empty-secret":
		v := f.waVectors["simple-json"]
		return VerifierConfig{}, []byte(v.RawBody), waHeader(v.ExpectedSignatureHeader), ""
	case "wa-nonhex-digest":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader(whatsappSigPrefix + strings.Repeat("zz", 32)), ""
	case "wa-truncated-digest":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader(v.ExpectedSignatureHeader[:len(v.ExpectedSignatureHeader)-8]), ""
	case "wa-wrong-prefix":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader("sha1=" + strings.Repeat("ab", 32)), ""
	case "wa-trailing-newline":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody + "\n"), waHeader(v.ExpectedSignatureHeader), ""
	case "wa-uppercase-hex":
		v := f.waVectors["simple-json"]
		upper := whatsappSigPrefix + strings.ToUpper(strings.TrimPrefix(v.ExpectedSignatureHeader, whatsappSigPrefix))
		return waCfg, []byte(v.RawBody), waHeader(upper), ""
	case "wa-first-byte-flipped":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader(flip(v.ExpectedSignatureHeader, 0)), ""
	case "wa-last-byte-flipped":
		v := f.waVectors["simple-json"]
		return waCfg, []byte(v.RawBody), waHeader(flip(v.ExpectedSignatureHeader, sha256.Size-1)), ""

	// ── Africa's Talking ─────────────────────────────────────────────────
	case "at-allowlist-match":
		cfg := VerifierConfig{ATAllowedCIDRs: []string{"196.250.209.0/24", "10.0.0.0/8"}}
		return cfg, []byte(`{"notification":"delivered"}`), http.Header{"X-Forwarded-For": []string{"196.250.209.77, 10.10.0.9"}}, ""
	case "at-outside-allowlist":
		return atCfg, []byte(`{"notification":"delivered"}`), http.Header{"X-Forwarded-For": []string{"203.0.113.7"}}, ""
	case "at-missing-peer":
		return atCfg, []byte(`{"notification":"delivered"}`), http.Header{}, ""
	case "at-garbage-peer":
		return atCfg, []byte(`{"notification":"delivered"}`), http.Header{"X-Forwarded-For": []string{"not-an-ip"}}, ""
	case "at-malformed-cidr":
		cfg := VerifierConfig{ATAllowedCIDRs: []string{"10.0.0.1/8"}} // host bits set → entry disabled
		return cfg, []byte(`{"notification":"delivered"}`), http.Header{"X-Forwarded-For": []string{"10.0.0.1"}}, ""
	case "at-forged-xff-uncompliant-edge":
		// Client-asserted leftmost entry claiming an allowed IP; the
		// rightmost-entry rule discards it.
		return atCfg, []byte(`{"notification":"delivered"}`), http.Header{"X-Forwarded-For": []string{"10.0.0.1, 203.0.113.7"}}, ""
	case "at-unconfigured-allowlist":
		return VerifierConfig{}, []byte(`{"notification":"delivered"}`), http.Header{"X-Forwarded-For": []string{"203.0.113.7"}}, ""
	}
	t.Fatalf("no scenario for attack case %q", id)
	return VerifierConfig{}, nil, nil, ""
}

func TestAttackMatrix(t *testing.T) {
	var matrix struct {
		Cases []attackCase `json:"cases"`
	}
	loadJSONFixture(t, "attack_matrix.json", &matrix)
	if len(matrix.Cases) < 25 {
		t.Fatalf("attack matrix unexpectedly small: %d cases", len(matrix.Cases))
	}
	f := loadAttackFixture(t)

	for _, tc := range matrix.Cases {
		tc := tc
		t.Run(tc.ID, func(t *testing.T) {
			cfg, body, header, externalURL := f.scenario(t, tc.ID)

			var warns warnRecorder
			cfg.Logger = warns.logger

			var err error
			switch tc.Provider {
			case "twilio":
				err = VerifyTwilio(cfg, body, header, externalURL)
			case "whatsappcloud":
				err = VerifyWhatsAppCloud(cfg, body, header, externalURL)
			case "africastalking":
				err = VerifyAfricasTalking(cfg, body, header, externalURL)
			default:
				t.Fatalf("unknown provider %q", tc.Provider)
			}

			switch tc.Expect {
			case "accept":
				if err != nil {
					t.Fatalf("attack control case must pass (%s): %v", tc.Attack, err)
				}
			case "reject":
				if err == nil {
					t.Fatalf("attack case must be rejected (fail-closed) (%s): got nil", tc.Attack)
				}
				appErr, ok := err.(*apperrors.Error)
				if !ok || appErr.Kind != apperrors.KindUnauth {
					t.Fatalf("attack rejection must be 401-class (%s): %v", tc.Attack, err)
				}
			case "accept-with-warn":
				if err != nil {
					t.Fatalf("expected accept-with-warn (%s): %v", tc.Attack, err)
				}
				if warns.count() == 0 {
					t.Fatalf("expected a structured residual-risk warning (%s)", tc.Attack)
				}
			default:
				t.Fatalf("unknown expectation %q", tc.Expect)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Secret-leakage sweep: rejection paths return fixed strings and the
// structured warnings carry metadata only — no credential, payload or header
// content may ever surface in errors or logs.
// ─────────────────────────────────────────────────────────────────────────────

func TestNoSecretLeakage(t *testing.T) {
	const (
		canaryToken  = "CANARY-twilio-auth-token-3f9a1d"
		canarySecret = "CANARY-whatsapp-app-secret-91b2c4"
		canaryBody   = "CANARY-payload-marker-7cc2"
		canarySig    = "CANARY-signature-marker-5e01"
	)

	canaries := []string{canaryToken, canarySecret, canaryBody, canarySig}

	var warns warnRecorder
	cfg := VerifierConfig{
		TwilioAuthTokens:  []string{canaryToken},
		WhatsAppAppSecret: canarySecret,
		ATAllowedCIDRs:    []string{"10.0.0.0/8"},
		ExternalURL:       "https://webhook.orvexa.example/v1/webhooks",
		Logger:            warns.logger,
	}

	body := []byte(`{"secret_marker":"` + canaryBody + `"}`)
	scenarios := []struct {
		name  string
		run   func() error
		scope string
	}{
		{"twilio-wrong-sig", func() error {
			return VerifyTwilio(cfg, body, http.Header{TwilioSignatureHeader: []string{base64.StdEncoding.EncodeToString(make([]byte, 20))}}, cfg.ExternalURL)
		}, "twilio"},
		{"twilio-missing-url", func() error {
			return VerifyTwilio(cfg, body, http.Header{TwilioSignatureHeader: []string{canarySig}}, "")
		}, "twilio"},
		{"whatsapp-wrong-sig", func() error {
			return VerifyWhatsAppCloud(cfg, body, waHeader(waSign("attacker", body)), "")
		}, "whatsapp"},
		{"whatsapp-garbage-header", func() error {
			return VerifyWhatsAppCloud(cfg, body, waHeader("sha256=zz"), "")
		}, "whatsapp"},
		{"at-outside-allowlist", func() error {
			return VerifyAfricasTalking(cfg, body, http.Header{"X-Forwarded-For": []string{"203.0.113.7"}}, "")
		}, "africastalking"},
		{"at-unconfigured", func() error {
			return VerifyAfricasTalking(VerifierConfig{Logger: warns.logger}, body, http.Header{"X-Forwarded-For": []string{"203.0.113.7"}}, "")
		}, "africastalking"},
	}

	for _, s := range scenarios {
		s := s
		t.Run("error/"+s.name, func(t *testing.T) {
			err := s.run()
			if err == nil {
				return // acceptance paths leak nothing by construction; warn sweep below
			}
			msg := err.Error()
			for _, c := range canaries {
				if strings.Contains(msg, c) {
					t.Fatalf("error message leaks canary %q: %q", c, msg)
				}
			}
			if len(msg) > 120 {
				t.Fatalf("error message suspiciously long (possible request echo): %q", msg)
			}
		})
	}

	// Warnings: structured metadata only.
	t.Run("warn/no-request-data", func(t *testing.T) {
		for i, msg := range warns.msgs {
			for _, c := range canaries {
				if strings.Contains(msg, c) {
					t.Fatalf("warning %d leaks canary %q", i, c)
				}
			}
			for _, kv := range warns.kvs[i] {
				s, ok := kv.(string)
				if !ok {
					continue
				}
				for _, c := range canaries {
					if strings.Contains(s, c) {
						t.Fatalf("warning kv %d leaks canary %q", i, c)
					}
				}
			}
		}
	})

	// Gateway-level rejection: fixed message, no echo of what failed.
	t.Run("gateway/rejection-fixed-message", func(t *testing.T) {
		g := NewGateway(nil, "legacy-secret")
		g.SetVerifiers(Registry{ProviderTwilio: VerifyTwilio}, cfg)
		_, err := g.IngestHeaders(context.Background(), "twilio", body, http.Header{TwilioSignatureHeader: []string{canarySig}})
		if err == nil {
			t.Fatal("expected rejection")
		}
		appErr := err.(*apperrors.Error)
		if appErr.Message != "signature validation failed" {
			t.Fatalf("rejection message must be the fixed string, got %q", appErr.Message)
		}
		for _, c := range canaries {
			if strings.Contains(err.Error(), c) {
				t.Fatalf("gateway error leaks canary %q", c)
			}
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Registry & gateway wiring: normalization, precedence, legacy fallback —
// the existing X-Orvexa-Signature behavior must be byte-stable whenever no
// verifier is registered for a provider.
// ─────────────────────────────────────────────────────────────────────────────

func TestDefaultRegistryCoversIssue24Providers(t *testing.T) {
	reg := DefaultRegistry()
	for _, p := range []string{ProviderTwilio, ProviderWhatsAppCloud, ProviderAfricasTalking} {
		if reg[p] == nil {
			t.Fatalf("DefaultRegistry missing %q", p)
		}
	}
	if len(reg) != 3 {
		t.Fatalf("DefaultRegistry must have exactly the three issue-#24 providers, got %d", len(reg))
	}
}

func TestSetVerifiersNormalizesAndDropsNil(t *testing.T) {
	g := NewGateway(nil, "legacy-secret")
	g.SetVerifiers(Registry{
		"Twilio":          VerifyTwilio,
		" WHATSAPPCLOUD ": VerifyWhatsAppCloud,
		"africastalking":  nil, // dropped: a nil entry must never be routable
	}, VerifierConfig{})

	if _, ok := g.verifiers[ProviderTwilio]; !ok {
		t.Fatal("key 'Twilio' must normalize to 'twilio'")
	}
	if _, ok := g.verifiers[ProviderWhatsAppCloud]; !ok {
		t.Fatal("key ' WHATSAPPCLOUD ' must normalize to 'whatsappcloud'")
	}
	if _, ok := g.verifiers[ProviderAfricasTalking]; ok {
		t.Fatal("nil entry must be dropped, not registered")
	}

	// Clearing: SetVerifiers(nil, …) restores pure legacy behavior.
	g.SetVerifiers(nil, VerifierConfig{})
	if len(g.verifiers) != 0 {
		t.Fatalf("nil registry must clear verifiers, got %d", len(g.verifiers))
	}
}

func TestGatewayVerifierRoutingAndLegacyFallback(t *testing.T) {
	f := loadAttackFixture(t)
	v := f.twVectors["basic-multi-param"]
	cfg := VerifierConfig{TwilioAuthTokens: f.twTokens, ExternalURL: v.URL}

	legacyBody := []byte(`{"event":"ringing","nonce":"legacy-1"}`)
	legacySig := ComputeSignature("legacy-secret", legacyBody)
	legacyHeader := http.Header{SignatureHeader: []string{legacySig}}

	t.Run("verifier-takes-precedence-for-registered-provider", func(t *testing.T) {
		g := NewGatewayWithVerifiers(nil, "legacy-secret", DefaultRegistry(), cfg)
		twHeader := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}

		// Authentic carrier delivery: verifier accepts.
		if !g.authorized("twilio", []byte(v.FormBody), twHeader) {
			t.Fatal("valid Twilio delivery must be authorized via the verifier")
		}
		// Case-insensitive provider path: same verifier.
		if !g.authorized("Twilio", []byte(v.FormBody), twHeader) {
			t.Fatal("provider path normalization must reach the same verifier")
		}
		// A platform-signed delivery for a REGISTERED provider must go
		// through the verifier (which rejects: no carrier signature).
		if g.authorized("twilio", legacyBody, legacyHeader) {
			t.Fatal("registered provider must not fall back to the legacy check")
		}
		// Tampered carrier delivery: verifier rejects.
		if g.authorized("twilio", []byte(v.FormBody+"\n"), twHeader) {
			t.Fatal("tampered Twilio delivery must be rejected")
		}
	})

	t.Run("legacy-path-preserved-for-unregistered-provider", func(t *testing.T) {
		g := NewGatewayWithVerifiers(nil, "legacy-secret", DefaultRegistry(), cfg)
		if !g.authorized("simulator", legacyBody, legacyHeader) {
			t.Fatal("unregistered provider must keep the legacy X-Orvexa-Signature path")
		}
		bad := http.Header{SignatureHeader: []string{ComputeSignature("wrong", legacyBody)}}
		if g.authorized("simulator", legacyBody, bad) {
			t.Fatal("wrong legacy signature must be rejected")
		}
		// Missing legacy header on the fallback path: reject.
		if g.authorized("simulator", legacyBody, http.Header{}) {
			t.Fatal("missing legacy signature must be rejected")
		}
	})

	t.Run("no-verifiers-is-byte-stable-legacy", func(t *testing.T) {
		g0 := NewGateway(nil, "legacy-secret")
		if !g0.authorized("twilio", legacyBody, legacyHeader) {
			t.Fatal("default gateway must validate legacy signatures for ANY provider")
		}
		twHeader := http.Header{TwilioSignatureHeader: []string{v.ExpectedSignatureHeader}}
		if g0.authorized("twilio", []byte(v.FormBody), twHeader) {
			t.Fatal("default gateway must not accept carrier signatures (no verifier installed)")
		}
		if g0.authorized("twilio", legacyBody, http.Header{}) {
			t.Fatal("default gateway must reject missing signatures")
		}
		// (The empty-provider guard lives at the Ingest/IngestHeaders entry
		// points — covered by TestIngestHeadersGuardrailsBeforeAuth.)
	})
}

func TestIngestHeadersGuardrailsBeforeAuth(t *testing.T) {
	g := NewGatewayWithVerifiers(nil, "legacy-secret", DefaultRegistry(), VerifierConfig{TwilioAuthTokens: []string{"t"}})
	ctx := context.Background()

	if _, err := g.IngestHeaders(ctx, "", []byte(`{}`), nil); err == nil {
		t.Fatal("empty provider must be rejected")
	} else if err.(*apperrors.Error).Kind != apperrors.KindInvalid {
		t.Fatalf("empty provider must be Invalid-class, got %v", err)
	}
	long := strings.Repeat("p", 65)
	if _, err := g.IngestHeaders(ctx, long, []byte(`{}`), nil); err == nil {
		t.Fatal("provider path >64 chars must be rejected")
	}
	if _, err := g.IngestHeaders(ctx, "twilio", nil, nil); err == nil {
		t.Fatal("empty body must be rejected")
	}
	big := make([]byte, MaxPayloadBytes+1)
	if _, err := g.IngestHeaders(ctx, "twilio", big, nil); err == nil {
		t.Fatal("oversized body must be rejected")
	}
	// A nil header map is treated as empty → verifier fails closed.
	if _, err := g.IngestHeaders(ctx, "twilio", []byte(`{}`), nil); err == nil {
		t.Fatal("nil headers with registered verifier must reject (fail-closed)")
	} else if err.(*apperrors.Error).Kind != apperrors.KindUnauth {
		t.Fatalf("verifier rejection must be Unauth-class, got %v", err)
	}
}

func TestIngestKeepsLegacyBehaviorForUnregisteredProviders(t *testing.T) {
	// The legacy Ingest entry point: registered providers route through the
	// verifier (rejecting platform-signed bodies without DB access), while
	// unregistered providers keep exact legacy semantics (validated here up
	// to the authorization boundary; persistence is covered by the
	// integration suite).
	g := NewGatewayWithVerifiers(nil, "legacy-secret", DefaultRegistry(), VerifierConfig{
		TwilioAuthTokens: []string{"t"}, ExternalURL: "https://webhook.orvexa.example",
	})
	body := []byte(`{"event":"ringing","nonce":"x-1"}`)

	if _, err := g.Ingest(context.Background(), "twilio", body, ComputeSignature("legacy-secret", body)); err == nil {
		t.Fatal("registered provider must be verified by its verifier, not the legacy check")
	} else if err.(*apperrors.Error).Kind != apperrors.KindUnauth {
		t.Fatalf("expected Unauth, got %v", err)
	}

	g2 := NewGateway(nil, "legacy-secret") // no verifiers: pure legacy
	// Note: accept-path persistence needs a DB (integration suite); here we
	// pin the authorization result only.
	if !g2.authorized("twilio", body, http.Header{SignatureHeader: []string{ComputeSignature("legacy-secret", body)}}) {
		t.Fatal("legacy gateway must accept valid platform signatures")
	}
}

func TestVerifyFuncContract(t *testing.T) {
	// The verifier slot receives the EXACT raw body, the full header map and
	// the resolved external URL — pin the contract adapters rely on.
	var gotBody []byte
	var gotHeader http.Header
	var gotURL string
	probe := func(cfg VerifierConfig, rawBody []byte, header http.Header, externalURL string) error {
		gotBody = rawBody
		gotHeader = header
		gotURL = externalURL
		return nil
	}

	g := NewGateway(nil, "legacy-secret")
	g.SetVerifiers(Registry{"Twilio": probe}, VerifierConfig{
		ExternalURL:  "https://webhook.orvexa.example/shared",
		ExternalURLs: map[string]string{"twilio": "https://webhook.orvexa.example/twilio"},
	})

	body := []byte(`{"probe":true}`)
	header := http.Header{"X-Twilio-Signature": []string{"abc"}, "X-Custom-Trace": []string{"keep-me"}}
	if !g.authorized("Twilio", body, header) {
		t.Fatal("probe verifier must be consulted and can accept")
	}
	if string(gotBody) != string(body) {
		t.Fatalf("verifier must receive the exact raw body, got %q", gotBody)
	}
	if gotHeader.Get("X-Custom-Trace") != "keep-me" || gotHeader.Get("X-Twilio-Signature") != "abc" {
		t.Fatal("verifier must receive the full delivery header set")
	}
	if gotURL != "https://webhook.orvexa.example/twilio" {
		t.Fatalf("verifier must receive the per-provider external URL, got %q", gotURL)
	}
}

func TestVerifierConfigExternalURLPrecedence(t *testing.T) {
	cfg := VerifierConfig{
		ExternalURL:  "https://shared.orvexa.example/base",
		ExternalURLs: map[string]string{"Twilio": "https://twilio.orvexa.example/callback"},
	}
	if got := cfg.externalURLFor("twilio"); got != "https://twilio.orvexa.example/callback" {
		t.Fatalf("per-provider override must win, got %q", got)
	}
	if got := cfg.externalURLFor("whatsappcloud"); got != "https://shared.orvexa.example/base" {
		t.Fatalf("providers without override must get the shared default, got %q", got)
	}

	// Empty override falls back to the shared default; keys are matched
	// normalized.
	cfg2 := VerifierConfig{
		ExternalURL:  "https://shared.orvexa.example/base",
		ExternalURLs: map[string]string{"twilio": ""},
	}
	if got := cfg2.externalURLFor("Twilio"); got != "https://shared.orvexa.example/base" {
		t.Fatalf("empty override must fall back to shared default, got %q", got)
	}

	// No configuration at all: empty (Twilio then fails closed).
	if got := (VerifierConfig{}).externalURLFor("twilio"); got != "" {
		t.Fatalf("unconfigured external URL must be empty, got %q", got)
	}
}

// TestVerifierIsolationFromLegacySecret: a verifier installation must not
// change how the LEGACY path validates unrelated providers (no shared state,
// no accidental cross-provider trust).
func TestVerifierIsolationFromLegacySecret(t *testing.T) {
	f := loadAttackFixture(t)
	v := f.waVectors["simple-json"]
	g := NewGatewayWithVerifiers(nil, "legacy-secret", Registry{ProviderWhatsAppCloud: VerifyWhatsAppCloud},
		VerifierConfig{WhatsAppAppSecret: f.waSecret})

	// The WhatsApp secret must not validate legacy traffic and vice versa.
	waHeaderSet := waHeader(v.ExpectedSignatureHeader)
	if g.authorized("simulator", []byte(v.RawBody), waHeaderSet) {
		t.Fatal("carrier signature must not authorize the legacy path of another provider")
	}
	legacySig := ComputeSignature(f.waSecret, []byte(v.RawBody))
	if g.authorized("simulator", []byte(v.RawBody), http.Header{SignatureHeader: []string{legacySig}}) {
		t.Fatal("HMAC under the carrier secret must not authorize the legacy path")
	}
	// And an HMAC under the legacy secret must not authorize the verifier
	// path either: the verifier only accepts its own scheme's MAC.
	legacyMac := waSign("legacy-secret", []byte(v.RawBody))
	if g.authorized("whatsappcloud", []byte(v.RawBody), waHeader(legacyMac)) {
		t.Fatal("HMAC under the legacy secret must not authorize the verifier path")
	}
}
