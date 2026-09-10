package whatsappcloud

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// This file maps Graph API failures onto Orvexa's application error model
// (pkg/errors). The full code-level taxonomy with documented semantics
// (24h window, rate limits, deliverability, auth) is defined here; every
// mapped error is an *apperrors.Error (so the httpx envelope renders it) or
// a typed error unwrapping to one (so errors.As reaches both).

// graphErrorBody is the Graph API error envelope:
//
//	{"error":{"message":"...","type":"OAuthException","code":131047,
//	          "error_data":{"messaging_product":"whatsapp","details":"..."},
//	          "fbtrace_id":"..."}}
type graphErrorBody struct {
	Error *graphError `json:"error"`
}

// graphError is one Graph error entry. Code/Type/Details are operator
// diagnostics; none of them ever carry credential material (the access token
// travels only in OUR request header and is never echoed by Graph).
type graphError struct {
	Message      string `json:"message"`
	Type         string `json:"type"`
	Code         int    `json:"code"`
	ErrorSubcode int    `json:"error_subcode"`
	FBTraceID    string `json:"fbtrace_id"`
	ErrorData    *struct {
		MessagingProduct string `json:"messaging_product"`
		Details          string `json:"details"`
	} `json:"error_data"`
}

// details renders the provider-side detail string when present.
func (g *graphError) details() string {
	if g == nil || g.ErrorData == nil {
		return ""
	}
	return g.ErrorData.Details
}

// parseGraphError decodes the envelope leniently: a non-Graph error body
// (HTML error page, empty body) yields nil and the caller classifies by
// HTTP status alone.
func parseGraphError(body []byte) *graphError {
	var env graphErrorBody
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return nil
	}
	return env.Error
}

// ProviderError is the typed catch-all for Graph failures without a
// dedicated classification. It carries the provider diagnostics (HTTP
// status, Graph code/type, fbtrace id, Meta's message) for operators while
// the client-safe surface stays a fixed, opaque message — internals never
// leak through KindInternal.
type ProviderError struct {
	HTTPStatus int
	Code       int    // Graph error code (0 when the body carried none)
	ErrType    string // Graph error type, e.g. "OAuthException"
	Message    string // Meta's message text (no credential material)
	Details    string // error_data.details when present
	FBTraceID  string // Meta support trace id

	appErr *apperrors.Error
}

// Error renders the operator-facing form: code, classification and Meta's
// message. Safe for logs; contains no credential material by construction.
func (e *ProviderError) Error() string {
	return fmt.Sprintf("whatsapp.provider_error: graph status=%d code=%d type=%q trace=%s: %s",
		e.HTTPStatus, e.Code, e.ErrType, e.FBTraceID, e.Message)
}

// Unwrap exposes the application error (KindInternal, code
// whatsapp.provider_error) so errors.As reaches the taxonomy.
func (e *ProviderError) Unwrap() error { return e.appErr }

// mapGraphError classifies one failed HTTP response into the apperrors
// taxonomy and its retry verdict.
//
// Semantics (issue #27; codes verified against Meta's Cloud API error
// reference):
//
//   - 131047 — free-form send outside the 24-hour customer service window:
//     typed *WindowClosedError (KindConflict, code whatsapp.window_closed);
//     NEVER retried — re-sending the same free-form body cannot succeed.
//   - 130429 / 80007 — rate limit hit: KindRateLimited
//     (whatsapp.rate_limited); retryable within the bounded budget.
//   - 131026 — message undeliverable (recipient cannot receive WhatsApp):
//     KindInvalid (whatsapp.undeliverable); never retried — permanent.
//   - 190 / HTTP 401 — access token invalid or expired: KindUnauth
//     (whatsapp.auth_failed); never retried — re-sending cannot fix auth.
//   - HTTP 429 without a recognized code — rate limited by status:
//     KindRateLimited; retryable.
//   - HTTP 5xx — provider-side transient: typed *ProviderError
//     (KindInternal); retryable.
//   - everything else — typed *ProviderError with the Graph code preserved
//     (KindInternal, deliberately conservative: an unknown 4xx may be caller
//     input or provider policy, and mislabeling it KindInvalid would blame
//     the caller without evidence).
func mapGraphError(status int, body []byte, header http.Header) *graphAttempt {
	ge := parseGraphError(body)

	switch {
	case ge != nil && ge.Code == codeWindowClosed:
		return &graphAttempt{err: newWindowClosedError(ge), retryable: false}
	case ge != nil && (ge.Code == codeRateLimitHit || ge.Code == codeRateLimitLegacy):
		after := retryAfterSeconds(header.Get("Retry-After"))
		return &graphAttempt{err: newRateLimitedError(ge, after), retryable: true, retryAfter: after}
	case ge != nil && ge.Code == codeUndeliverable:
		return &graphAttempt{err: newUndeliverableError(ge), retryable: false}
	case (ge != nil && ge.Code == codeAuthFailed) || status == http.StatusUnauthorized:
		return &graphAttempt{err: newAuthFailedError(ge), retryable: false}
	case status == http.StatusTooManyRequests:
		// 429 with an unrecognized code: trust the status.
		after := retryAfterSeconds(header.Get("Retry-After"))
		return &graphAttempt{err: newRateLimitedError(ge, after), retryable: true, retryAfter: after}
	case status >= 500:
		return &graphAttempt{err: providerError(status, ge), retryable: true}
	default:
		return &graphAttempt{err: providerError(status, ge), retryable: false}
	}
}

// providerError builds the conservative catch-all for a failed response.
func providerError(status int, ge *graphError) error {
	pe := &ProviderError{
		HTTPStatus: status,
		ErrType:    "unknown",
		FBTraceID:  "unknown",
	}
	if ge != nil {
		pe.Code = ge.Code
		pe.ErrType = nonEmpty(ge.Type, "unknown")
		pe.Message = ge.Message
		pe.Details = ge.details()
		pe.FBTraceID = nonEmpty(ge.FBTraceID, "unknown")
	} else {
		pe.Message = fmt.Sprintf("provider returned HTTP %d with a non-Graph error body", status)
	}
	pe.appErr = apperrors.Internal("whatsapp.provider_error",
		"provider rejected the request").WithCause(pe)
	return pe
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// The Graph error codes with dedicated semantics. Aliases below keep the
// mapping table readable at the call site.
const (
	// codeWindowClosed: "Re-engagement message" — a free-form (non-template)
	// send arrived more than 24 hours after the customer's last message.
	codeWindowClosed = 131047
	// codeRateLimitHit: Cloud API rate limit hit (the current code).
	codeRateLimitHit = 130429
	// codeRateLimitLegacy: legacy Sp-message rate limit code still observed
	// on some surfaces.
	codeRateLimitLegacy = 80007
	// codeUndeliverable: message undeliverable — recipient phone number is
	// not on WhatsApp / cannot receive messages.
	codeUndeliverable = 131026
	// codeAuthFailed: access token invalid, expired or revoked.
	codeAuthFailed = 190
)

// WindowClosedError is returned when a FREE-FORM send (text or media via
// Send) is attempted outside the 24-hour customer service window (Graph
// code 131047): Meta only allows free-form messaging within 24 hours of the
// customer's last message to the business number.
//
// Semantics (documented contract):
//
//   - Kind is conflict (409 class): the request is well-formed but conflicts
//     with the conversation's current state — the window is closed. The
//     send is failed, never queued; re-sending the same body cannot succeed
//     (which is also why this error is never retried).
//   - The typed error is reachable via errors.As(err, *WindowClosedError);
//     the application error (code whatsapp.window_closed, KindConflict) is
//     reachable via errors.As(err, *apperrors.Error) — one error, both
//     surfaces, so runbooks and API clients key on the same machine code.
//   - Documented escape: send an approved TEMPLATE instead
//     (Provider.SendTemplate). Meta delivers template messages outside the
//     window by design (that is what the window exists to enforce —
//     businesses re-engage through reviewed templates, not ad-hoc text).
//
// This is deliberately an explicit typed error, not a silent template
// fallback: silently converting a caller's free-form message into a
// different message type would hide a product decision that belongs to the
// caller.
type WindowClosedError struct {
	// ProviderCode is the Graph code (131047).
	ProviderCode int
	// Detail is Meta's error_data.details text when present (e.g. "Message
	// failed to send because more than 24 hours have passed since the
	// customer last replied to this number"). Operator-side only — never
	// merged into the client-safe message.
	Detail string
	// FBTraceID is Meta's support trace id.
	FBTraceID string

	appErr *apperrors.Error
}

// Error renders the application surface: stable code plus the safe message
// that documents the template escape.
func (e *WindowClosedError) Error() string { return e.appErr.Error() }

// Unwrap exposes the application error (KindConflict,
// whatsapp.window_closed) for errors.As.
func (e *WindowClosedError) Unwrap() error { return e.appErr }

func newWindowClosedError(ge *graphError) error {
	return &WindowClosedError{
		ProviderCode: ge.Code,
		Detail:       ge.details(),
		FBTraceID:    ge.FBTraceID,
		appErr: apperrors.Conflict("whatsapp.window_closed",
			"free-form messaging is only allowed within 24 hours of the customer's last message; send an approved template instead (SendTemplate)"),
	}
}

// RateLimitedError is the typed form of a provider rate-limit rejection
// (Graph 130429 / 80007, or HTTP 429 without a recognized code). Kind is
// rate_limited (429 class); the adapter retries these within its bounded
// budget before surfacing, so a returned RateLimitedError means the budget
// was exhausted or the context expired — callers should back off on their
// side (the core's send path fails the interaction; a human-visible rate
// limit is better than a silently dropped message).
type RateLimitedError struct {
	// ProviderCode is the Graph code (130429, 80007, or 0 when only the
	// HTTP status signaled the rate limit).
	ProviderCode int
	// RetryAfter is the provider's Retry-After hint (0 = none). The
	// adapter already honored it, bounded by RetryPolicy.MaxDelay.
	RetryAfter time.Duration
	// FBTraceID is Meta's support trace id.
	FBTraceID string

	appErr *apperrors.Error
}

// Error renders the application surface.
func (e *RateLimitedError) Error() string { return e.appErr.Error() }

// Unwrap exposes the application error (KindRateLimited,
// whatsapp.rate_limited) for errors.As.
func (e *RateLimitedError) Unwrap() error { return e.appErr }

func newRateLimitedError(ge *graphError, after time.Duration) error {
	re := &RateLimitedError{RetryAfter: after}
	if ge != nil {
		re.ProviderCode = ge.Code
		re.FBTraceID = ge.FBTraceID
	}
	re.appErr = apperrors.RateLimited("whatsapp.rate_limited",
		"provider rate limit exceeded; retry with backoff")
	return re
}

// UndeliverableError marks a permanent deliverability failure (Graph
// 131026): the recipient phone number cannot receive WhatsApp messages
// (not on WhatsApp, wrong number, blocked business). Kind is invalid (422
// class): the destination, not the transport, is the problem, and retrying
// is pointless — the interaction's next step is agent follow-up on the
// contact record, not another send.
type UndeliverableError struct {
	// ProviderCode is the Graph code (131026).
	ProviderCode int
	// Detail is Meta's error_data.details text when present.
	Detail string
	// FBTraceID is Meta's support trace id.
	FBTraceID string

	appErr *apperrors.Error
}

// Error renders the application surface.
func (e *UndeliverableError) Error() string { return e.appErr.Error() }

// Unwrap exposes the application error (KindInvalid,
// whatsapp.undeliverable) for errors.As.
func (e *UndeliverableError) Unwrap() error { return e.appErr }

func newUndeliverableError(ge *graphError) error {
	return &UndeliverableError{
		ProviderCode: ge.Code,
		Detail:       ge.details(),
		FBTraceID:    ge.FBTraceID,
		appErr: apperrors.Invalid("whatsapp.undeliverable",
			"recipient cannot receive WhatsApp messages; verify the number before retrying"),
	}
}

// AuthFailedError marks a rejected credential (Graph 190 or HTTP 401): the
// access token is invalid, expired or revoked. Kind is unauthorized (401
// class). Never retried — re-sending with the same token cannot succeed;
// the fix is operational (rotate the system-user token), never in-process.
type AuthFailedError struct {
	// ProviderCode is the Graph code (190, or 0 when only the HTTP status
	// signaled the auth failure).
	ProviderCode int
	// FBTraceID is Meta's support trace id.
	FBTraceID string

	appErr *apperrors.Error
}

// Error renders the application surface. The message is a fixed string: it
// never contains the token, and Meta's raw message is deliberately not
// surfaced client-side.
func (e *AuthFailedError) Error() string { return e.appErr.Error() }

// Unwrap exposes the application error (KindUnauth, whatsapp.auth_failed)
// for errors.As.
func (e *AuthFailedError) Unwrap() error { return e.appErr }

func newAuthFailedError(ge *graphError) error {
	ae := &AuthFailedError{}
	if ge != nil {
		ae.ProviderCode = ge.Code
		ae.FBTraceID = ge.FBTraceID
	}
	ae.appErr = apperrors.Unauth("whatsapp.auth_failed",
		"provider rejected the access token; rotate the credential")
	return ae
}
