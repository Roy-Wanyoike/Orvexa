package webhooks

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"net/http"
	"net/url"
	"sort"
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// TwilioSignatureHeader is the header Twilio delivers its request validator
// signature in.
const TwilioSignatureHeader = "X-Twilio-Signature"

// twilio rejection codes are stable machine codes surfaced as 401-class
// application errors (pkg/errors taxonomy). They never embed request data.
const (
	twilioCodeMisconfigured    = "webhook.twilio_misconfigured"     // no tokens / no external URL
	twilioCodeInvalidSignature = "webhook.twilio_invalid_signature" // bad/missing/wrong signature
)

// VerifyTwilio implements Twilio's documented request-validator scheme for
// form-encoded webhooks (voice, SMS and Messaging status callbacks):
//
//	X-Twilio-Signature = base64(HMAC-SHA1(authToken, signedPayload))
//	signedPayload      = fullURL + concat(key + value for each POST param,
//	                     sorted ascending by key, ties broken by value,
//	                     WITHOUT separators, appended directly to the URL)
//
// The params are the URL-DECODED x-www-form-urlencoded values (a raw "+"
// decodes to a space, "%2B" to a literal "+"), and the sort is over UTF-8
// encoded keys in code-point order — identical to Go's byte-wise string sort
// (a property of the UTF-8 encoding; proven in verify_test.go against a
// Python reference and against an explicit code-point ordering).
//
// ── THE URL / PROXY RECONSTRUCTION PITFALL (read before wiring) ────────────
// Twilio signs the URL EXACTLY as its platform dialed it: scheme, external
// host, port (when non-default for the scheme), path AND query string. That
// URL is frequently NOT what the Orvexa process observes: a TLS-terminating
// proxy forwards http://internal-host:8080/..., so r.URL/r.Host/X-Forwarded-*
// give reconstruction guesses — and any mismatch (internal host, http vs
// https, missing query, wrong port) is an unconditional signature reject.
// Correctly fail-closed, but operationally the #1 Twilio integration trap.
// THEREFORE: externalURL MUST be the externally-routable webhook URL from
// configuration (VerifierConfig.ExternalURL / ExternalURLs[provider]), NOT
// derived from the inbound request. If Twilio was configured with a query
// string on the callback, the caller must include it in externalURL
// (config base + "?" + incoming RawQuery): the canonical validators sign the
// full dialed URL including the query. Never "fix" a mismatch by loosening
// the check — fix the configured URL.
//
// ── MULTIPLE AUTH TOKENS (rotation) ────────────────────────────────────────
// Twilio account credential rotation keeps TWO auth tokens live at once. All
// configured tokens are tried; a signature matching ANY of them validates
// (hmac.Equal per token — constant-time). Timing across the token loop is
// not secret-bearing: an invalid signature always traverses every token
// (identical total work), and only a signer who already holds a valid token
// can observe which token position matched.
//
// An empty token list fails closed (no tokens → nothing can validate).
// Empty-string entries inside the list are dropped the same way: HMAC-SHA1
// with an empty key is computable by anyone, so an empty-string token could
// never be allowed to validate a signature (an "unsigned path in disguise").
// If no non-empty token remains, the delivery rejects as misconfigured.
//
// ── SCOPE / RESIDUAL RISK ──────────────────────────────────────────────────
// This validates Twilio's classic form-encoded signing surface, the
// documented default for Twilio webhooks. A body that does not parse as
// x-www-form-urlencoded (e.g. Twilio's separate "JSON webhooks" mode) is
// REJECTED — adapters must point Twilio at the form-encoded callback mode.
func VerifyTwilio(cfg VerifierConfig, rawBody []byte, header http.Header, externalURL string) error {
	if externalURL == "" {
		// No external URL configured → signatures can never be evaluated
		// honestly. Reject rather than guess a URL from the request.
		return apperrors.Unauth(twilioCodeMisconfigured, "signature validation failed")
	}
	tokens := make([]string, 0, len(cfg.TwilioAuthTokens))
	for _, t := range cfg.TwilioAuthTokens {
		if t != "" {
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		// No tokens configured, or every configured token is the empty string.
		return apperrors.Unauth(twilioCodeMisconfigured, "signature validation failed")
	}

	provided, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header.Get(TwilioSignatureHeader)))
	if err != nil || len(provided) != sha1.Size {
		// Missing header, non-base64 garbage, and wrong-length digests all
		// land here: one fixed rejection, no request data echoed back.
		return apperrors.Unauth(twilioCodeInvalidSignature, "signature validation failed")
	}

	params, err := url.ParseQuery(string(rawBody))
	if err != nil {
		// Not a form-encoded body (JSON webhook mode, malformed escapes,
		// ';' separators): reject. Adapters must use Twilio's form mode.
		return apperrors.Unauth(twilioCodeInvalidSignature, "signature validation failed")
	}

	// Flatten to (key, value) pairs — including duplicate keys — and sort by
	// key with value as tie-breaker, matching the canonical validators'
	// sorted((key, value)) tuple ordering. Go compares strings byte-wise,
	// which for UTF-8 is Unicode code-point order (proven in tests).
	pairs := make([][2]string, 0, len(params))
	for k, vs := range params {
		for _, v := range vs {
			pairs = append(pairs, [2]string{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})

	var b strings.Builder
	b.Grow(len(externalURL) + len(rawBody))
	b.WriteString(externalURL)
	for _, p := range pairs {
		b.WriteString(p[0])
		b.WriteString(p[1])
	}
	signed := []byte(b.String())

	for _, token := range tokens {
		mac := hmac.New(sha1.New, []byte(token))
		mac.Write(signed)
		if hmac.Equal(mac.Sum(nil), provided) {
			return nil
		}
	}
	return apperrors.Unauth(twilioCodeInvalidSignature, "signature validation failed")
}
