// Provider webhook signature verification (issue #24, [O-15]).
//
// This file hosts the pluggable verifier registry: one VerifyFunc per real
// carrier, keyed by provider name, injectable into the Gateway without
// touching the platform's own X-Orvexa-Signature path (see gateway.go — the
// legacy behavior is byte-identical when no verifiers are installed).
//
// SECURITY POSTURE (applies to every verifier in this package):
//   - Fail-closed: a verifier that cannot establish authenticity rejects
//     (401-class application error); empty/misconfigured credentials reject
//     rather than degrade into an unsigned ingestion path.
//   - Constant-time: all MAC comparisons go through crypto/hmac's hmac.Equal
//     on raw MAC bytes (per-verifier doc comments carry the proof obligations
//     and what the tests demonstrate).
//   - No secrets in logs or errors: rejection messages are fixed strings;
//     structured logs carry only provider/peer metadata. Adversarially tested
//     in verify_test.go (TestNoSecretLeakage).
//
// Residual-risk documentation per provider lives in the respective
// verify_*.go file and in the package-level threat-model notes below.
package webhooks

import (
	"net/http"
	"strings"
)

// Built-in provider registry keys. The provider path segment of
// POST /v1/webhooks/{provider} is normalized (lowercased, trimmed) before the
// registry lookup, so wiring "Twilio" and "twilio" is equivalent.
const (
	ProviderTwilio         = "twilio"
	ProviderWhatsAppCloud  = "whatsappcloud"
	ProviderAfricasTalking = "africastalking"
)

// VerifyFunc validates ONE raw provider webhook delivery.
//
//	cfg          — static verifier configuration (credentials, allowlists).
//	rawBody      — the exact request body bytes received; verifiers must
//	               treat any body mutation after signing as a rejection.
//	header       — full delivery headers (signature headers are per-provider).
//	externalURL  — the EXTERNAL webhook URL the carrier dialed (see the Twilio
//	               proxy/URL-reconstruction pitfall in verify_twilio.go).
//
// A nil return means the delivery is authentic as far as the configured
// scheme can establish; any error rejects the delivery (fail-closed).
type VerifyFunc func(cfg VerifierConfig, rawBody []byte, header http.Header, externalURL string) error

// VerifierConfig carries the credentials and controls the provider verifiers
// consume. Verifiers read only their own slice of the struct; nothing here is
// ever logged.
type VerifierConfig struct {
	// Twilio: one or more auth tokens. Twilio credential rotation keeps two
	// tokens live simultaneously, so a signature matching ANY token in the
	// list validates. Empty list → fail-closed.
	TwilioAuthTokens []string

	// WhatsApp Cloud API: the app secret (Meta App Dashboard →
	// Configuration). Empty → fail-closed.
	WhatsAppAppSecret string

	// Africa's Talking: peer-IP allowlist (CIDRs). Configured → peer must
	// match, else reject (fail-closed). Unconfigured → allow + structured
	// WARN on every acceptance (honest residual risk; AT signs nothing).
	ATAllowedCIDRs []string

	// Header that carries the peer IP for the AT allowlist. MUST be a header
	// the trusted edge proxy overwrites (default "X-Forwarded-For", leftmost
	// entry). VerifyFunc sees transport headers, not the socket, so peer
	// identity is exactly as trustworthy as the edge that sets this header.
	ATPeerIPHeader string

	// ExternalURL is the externally-routable webhook base URL used for
	// signature schemes that cover the URL (Twilio). MUST come from
	// configuration, never reconstructed from the inbound request (proxies
	// rewrite scheme/host; see verify_twilio.go).
	ExternalURL string

	// ExternalURLs optionally overrides ExternalURL per provider key
	// (normalized). Precedence: per-provider > shared default.
	ExternalURLs map[string]string

	// Logger receives structured warnings (e.g. the AT no-allowlist residual
	// risk warning). Signature: slog.Logger.Warn-compatible so wiring is
	// `cfg.Logger = logger.Warn`. Nil → silent (still fail-closed where
	// applicable; the logger never gates security decisions).
	Logger func(msg string, kv ...any)
}

// log emits a structured warning when a logger is wired. Fixed message +
// metadata only; callers must not pass secrets.
func (c VerifierConfig) log(msg string, kv ...any) {
	if c.Logger != nil {
		c.Logger(msg, kv...)
	}
}

// externalURLFor resolves the external webhook URL for a provider: the
// per-provider override when present, else the shared default. Map keys are
// matched normalized (an "Twilio" key is equivalent to "twilio").
func (c VerifierConfig) externalURLFor(provider string) string {
	p := normalizeProvider(provider)
	if c.ExternalURLs != nil {
		if u, ok := c.ExternalURLs[p]; ok && u != "" {
			return u
		}
		for k, u := range c.ExternalURLs {
			if normalizeProvider(k) == p && u != "" {
				return u
			}
		}
	}
	return c.ExternalURL
}

// peerIPHeaderName resolves the peer-IP header for the AT allowlist.
func (c VerifierConfig) peerIPHeaderName() string {
	if c.ATPeerIPHeader != "" {
		return c.ATPeerIPHeader
	}
	return "X-Forwarded-For"
}

// normalizeProvider canonicalizes a provider path segment for registry keys.
func normalizeProvider(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}

// Registry maps a normalized provider name to its verifier. A nil Registry,
// or one without an entry for a provider, leaves that provider on the
// legacy X-Orvexa-Signature path (gateway behavior preserved).
type Registry map[string]VerifyFunc

// DefaultRegistry returns the issue-#24 provider verifiers. Deployments
// enable a provider by installing this registry (or a subset) plus a
// VerifierConfig via NewGatewayWithVerifiers / Gateway.SetVerifiers.
func DefaultRegistry() Registry {
	return Registry{
		ProviderTwilio:         VerifyTwilio,
		ProviderWhatsAppCloud:  VerifyWhatsAppCloud,
		ProviderAfricasTalking: VerifyAfricasTalking,
	}
}
