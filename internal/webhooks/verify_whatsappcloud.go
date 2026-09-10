package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// WhatsAppSignatureHeader is Meta's signature header for WhatsApp Cloud API
// webhooks. (Header names are case-insensitive on lookup via http.Header.Get.)
const WhatsAppSignatureHeader = "X-Hub-Signature-256"

// whatsappSigPrefix is the exact scheme prefix Meta sends before the digest.
const whatsappSigPrefix = "sha256="

// wa rejection codes (stable machine codes, 401-class, fixed messages).
const (
	waCodeMisconfigured    = "webhook.whatsapp_misconfigured"     // empty app secret
	waCodeInvalidSignature = "webhook.whatsapp_invalid_signature" // bad/missing/wrong signature
)

// VerifyWhatsAppCloud implements Meta's documented WhatsApp Cloud API webhook
// integrity scheme:
//
//	X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(appSecret, rawBody))>
//
// Meta signs the EXACT raw body bytes, so a body that differs by a single
// byte — appended field, reordered whitespace, trailing newline — rejects.
//
// ── TIMING-SAFETY PROOF (why raw-byte compare, not hex-string compare) ─────
//  1. The provided digest is hex-DECODED to raw MAC bytes BEFORE comparison.
//  2. Comparison uses hmac.Equal (crypto/hmac): a constant-time, length-
//     independent-on-equal-length byte comparison whose result does not
//     depend on where (or whether) the bytes first diverge.
//  3. hex.DecodeString runs only on attacker-supplied input. Its behavior
//     (and timing) is a function of the attacker's own bytes and is fully
//     resolved BEFORE any secret-dependent computation is compared; a decode
//     failure rejects outright. No path exists where timing of the secret-
//     bearing comparison influences the outcome or varies with secret bytes.
//  4. We deliberately do NOT compare hex strings: string comparison is not
//     constant-time, would double the compared length, and would be
//     case-sensitive. Decoding normalizes case instead — Meta sends
//     lowercase hex, and an uppercase encoding of the SAME MAC still
//     validates (equivalence-class test in verify_test.go demonstrates the
//     raw-byte comparison path).
//
// ── FAIL-CLOSED ────────────────────────────────────────────────────────────
// An empty/missing app secret rejects everything: HMAC with an empty key is
// trivially computable by anyone, so "verify anyway" would be an unsigned
// ingestion path in disguise.
//
// ── RESIDUAL RISK / SCOPE NOTES ────────────────────────────────────────────
// Meta also transmits a legacy X-Hub-Signature-1 (SHA-1) on some surfaces.
// It is IGNORED and never accepted as a fallback — only SHA-256 over the
// configured app secret is accepted. Handshake (GET hub.challenge)
// verification is a subscription-management concern of the adapter wave and
// is intentionally out of scope here.
func VerifyWhatsAppCloud(cfg VerifierConfig, rawBody []byte, header http.Header, _ string) error {
	if cfg.WhatsAppAppSecret == "" {
		return apperrors.Unauth(waCodeMisconfigured, "signature validation failed")
	}

	provided := header.Get(WhatsAppSignatureHeader)
	if len(provided) <= len(whatsappSigPrefix) {
		// Covers: header absent, bare "sha256", bare "sha256=" (empty digest).
		return apperrors.Unauth(waCodeInvalidSignature, "signature validation failed")
	}
	if !strings.EqualFold(provided[:len(whatsappSigPrefix)], whatsappSigPrefix) {
		// Wrong scheme marker (e.g. the legacy "sha1=" X-Hub-Signature-1
		// value pasted into the 256 header by a confused proxy).
		return apperrors.Unauth(waCodeInvalidSignature, "signature validation failed")
	}

	digest, err := hex.DecodeString(provided[len(whatsappSigPrefix):])
	if err != nil || len(digest) != sha256.Size {
		// Odd length, non-hex bytes, or wrong MAC size: reject before any
		// comparison. Attacker-controlled input only — no secret involved.
		return apperrors.Unauth(waCodeInvalidSignature, "signature validation failed")
	}

	mac := hmac.New(sha256.New, []byte(cfg.WhatsAppAppSecret))
	mac.Write(rawBody)
	if hmac.Equal(mac.Sum(nil), digest) {
		return nil
	}
	return apperrors.Unauth(waCodeInvalidSignature, "signature validation failed")
}
