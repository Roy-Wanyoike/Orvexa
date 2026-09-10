// Package twilio implements the telephony.VoiceProvider port over Twilio's
// Programmable Voice REST API (v2010-04-01, JSON flavor) using nothing but
// net/http — no Twilio SDK dependency (issue #25).
//
// Architecture (hexagonal, per internal/telephony/ports.go):
//
//	core use-cases ──> telephony.VoiceProvider ──> twilio.Adapter ──HTTP──> Twilio REST
//	                                                    │
//	                                                    └── native status callbacks ──> comms.ProviderEvent
//	                                                        delivered through the configured comms.IngestFunc
//	                                                        (production: applied post-verification by the
//	                                                        fail-closed webhook gateway, O-15; tests: the
//	                                                        conformance Recorder — the same path the built-in
//	                                                        Simulator uses)
//
// Lifecycle translation (events.go): queued/ringing -> call.ringing,
// in-progress -> call.connected, completed/no-answer/failed/busy/canceled ->
// call.ended carrying the native status as the hangup cause.
package twilio

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ProviderName is the provider label stamped on every event delivery — the
// webhook gateway routes and scopes by it.
const ProviderName = "twilio"

// REST API shape (JSON flavor: the .json suffix makes Twilio speak JSON for
// both requests and responses).
const (
	defaultBaseURL  = "https://api.twilio.com"
	apiCallsPath    = "/2010-04-01/Accounts/%s/Calls.json"
	apiCallPath     = "/2010-04-01/Accounts/%s/Calls/%s.json"
	responseCapByte = 1 << 20 // 1 MiB cap on REST response bodies
)

// Retry policy: at most 3 retries after the initial attempt (4 attempts
// total), only on 429/5xx and transport errors — never on 4xx, which are
// deterministic rejections. Backoff is exponential with full jitter, capped,
// and honors a bounded Retry-After hint.
const (
	maxRetries       = 3
	defaultRetryBase = 100 * time.Millisecond
	maxBackoffDelay  = 2 * time.Second
	maxRetryAfter    = 5 * time.Second
)

// Config is the Twilio voice adapter configuration. Credentials are
// registry.Secret so every rendering path (fmt verbs, logs, JSON) is redacted
// by construction; see the redaction tests in voice_test.go.
type Config struct {
	// AccountSID + AuthToken authenticate all REST calls via HTTP basic auth
	// (Twilio's documented scheme: AccountSID as username).
	AccountSID registry.Secret
	AuthToken  registry.Secret

	// BaseURL is the Twilio REST root. Empty selects the production API
	// (https://api.twilio.com); tests inject an httptest server URL.
	BaseURL string

	// TwiMLBaseURL is the application's TwiML entry point (the `Url`
	// parameter of POST /Calls): the document Twilio fetches and executes
	// when the call is answered. A per-call override may be passed via
	// ProviderOptions ("twilio_url"). Required for PlaceCall.
	TwiMLBaseURL string

	// CallbackBaseURL is the externally-routable URL Twilio POSTs voice
	// status callbacks to — in production the platform's fail-closed webhook
	// gateway endpoint (O-15 verifies X-Twilio-Signature BEFORE the adapter
	// sees the request). The adapter appends the interaction id and tenant
	// as passthrough query parameters so every callback is self-scoping.
	// Required for PlaceCall.
	CallbackBaseURL string

	// HoldTwiMLURL is the looping hold document (TwiML <Pause> +
	// <Redirect> back to this URL). Twilio has no native hold on a live
	// call leg: hold is a redirect to a document that keeps the leg parked
	// until Resume redirects it away. Required for Hold; without it Hold
	// returns a typed unsupported error rather than faking the semantics.
	HoldTwiMLURL string

	// ResumeURL is the TwiML target a resumed leg is redirected to (the
	// application's post-hold continuation document). Falls back to the
	// leg's original entry TwiML when unset.
	ResumeURL string

	// Ingest is the verified-event application port (comms.IngestFunc).
	// Production wiring applies events through the comms processor after
	// the webhook gateway has verified the native callback's signature;
	// test wiring hands in the conformance Recorder. Required for the
	// lifecycle translation to be observable.
	Ingest comms.IngestFunc

	// Signer stamps each delivered event body. Zero value selects the
	// package default: HMAC-SHA256 over the body keyed by the auth token.
	Signer comms.Signer

	// HTTPClient is injectable for tests (httptest) and transport tuning.
	// Zero value selects a client with a bounded timeout.
	HTTPClient *http.Client

	// RetryBaseDelay is the backoff base for retries. Zero selects 100ms.
	RetryBaseDelay time.Duration
}

// String renders the configuration with zero credential material, e.g.
// "twilio(account_sid=****cdef auth_token=****cdef base_url=https://api.twilio.com)".
func (c Config) String() string {
	parts := []string{
		"account_sid=" + registry.Redacted(string(c.AccountSID)),
		"auth_token=" + registry.Redacted(string(c.AuthToken)),
	}
	if c.BaseURL != "" {
		parts = append(parts, "base_url="+c.BaseURL)
	}
	if c.TwiMLBaseURL != "" {
		parts = append(parts, "twiml_base_url="+c.TwiMLBaseURL)
	}
	if c.CallbackBaseURL != "" {
		parts = append(parts, "callback_base_url="+c.CallbackBaseURL)
	}
	if c.HoldTwiMLURL != "" {
		parts = append(parts, "hold_twiml_url="+c.HoldTwiMLURL)
	}
	if c.ResumeURL != "" {
		parts = append(parts, "resume_url="+c.ResumeURL)
	}
	return "twilio(" + strings.Join(parts, " ") + ")"
}

// GoString renders the masked form so %#v cannot disclose credentials.
func (c Config) GoString() string { return c.String() }

// client is the raw Twilio REST transport: basic auth, JSON parsing, and the
// bounded jittered retry policy. It is shared by every voice operation.
type client struct {
	baseURL    string
	accountSID string
	authToken  string
	hc         *http.Client
	retryBase  time.Duration
}

// newClient builds the transport from the adapter configuration.
func newClient(cfg *Config) *client {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	retryBase := cfg.RetryBaseDelay
	if retryBase <= 0 {
		retryBase = defaultRetryBase
	}
	return &client{
		baseURL:    strings.TrimRight(base, "/"),
		accountSID: string(cfg.AccountSID),
		authToken:  string(cfg.AuthToken),
		hc:         hc,
		retryBase:  retryBase,
	}
}

// callResource is the slice of Twilio's call JSON the adapter consumes
// (success responses only; error bodies parse into apiError).
type callResource struct {
	SID    string `json:"sid"`
	Status string `json:"status"`
}

// apiError is Twilio's JSON error envelope:
//
//	{"code": 20003, "message": "Authenticate", "more_info": "https://...", "status": 401}
type apiError struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
	Status   int    `json:"status"`
}

// post sends one form-encoded REST call with retries on 429/5xx/transport
// errors only. The form is re-encoded per attempt (request bodies are not
// reusable) and every attempt is bounded by ctx.
func (c *client) post(ctx context.Context, path string, form url.Values) (*callResource, error) {
	endpoint := c.baseURL + path
	var (
		lastErr      error
		retryAfter   time.Duration
		retryAfterOK bool
	)
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, c.retryBase, attempt-1, retryAfter, retryAfterOK); err != nil {
				return nil, err
			}
			retryAfter, retryAfterOK = 0, false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, apperrors.Internal("twilio.request_build_failed", "could not build the provider request").WithCause(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.SetBasicAuth(c.accountSID, c.authToken)

		resp, err := c.hc.Do(req)
		if err != nil {
			// Transport-level failure: retryable within budget. The retry
			// surface is bounded and only applies to operations where
			// re-submission is safe — see the idempotency notes on the
			// voice operations in voice.go.
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, responseCapByte))
		closeErr := resp.Body.Close()
		if readErr != nil || closeErr != nil {
			lastErr = firstErr(readErr, closeErr)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var cr callResource
			if len(body) > 0 {
				if err := json.Unmarshal(body, &cr); err != nil {
					return nil, apperrors.Internal("twilio.bad_response", "twilio returned an unparseable response").WithCause(err)
				}
			}
			return &cr, nil
		}

		apiErr := parseAPIError(body)
		lastErr = classify(resp.StatusCode, apiErr)
		if !retryableStatus(resp.StatusCode) {
			return nil, lastErr
		}
		retryAfter, retryAfterOK = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return nil, wrapTransport(lastErr)
}

// firstErr picks the first non-nil of two errors.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// wrapTransport renders an exhausted-retry or transport failure through the
// application error model (internal: nothing the caller can fix by resending
// differently). The cause is preserved for server-side logging; an already
// classified application error passes through unchanged.
func wrapTransport(err error) *apperrors.Error {
	if err == nil {
		return apperrors.Internal("twilio.request_failed", "twilio request failed")
	}
	var ae *apperrors.Error
	if asAppError(err, &ae) {
		return ae
	}
	return apperrors.Wrap(err, apperrors.KindInternal, "twilio.request_failed", "twilio request failed after retries")
}

// asAppError is errors.As specialized for *apperrors.Error (kept local so the
// adapter does not grow a stdlib errors dependency surface beyond wrapping).
func asAppError(err error, target **apperrors.Error) bool {
	for err != nil {
		if ae, ok := err.(*apperrors.Error); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// retryableStatus reports whether an HTTP status warrants a retry: rate
// limiting and provider-side 5xx failures only. Deterministic 4xx
// rejections (validation, auth, not-found) are never retried.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// parseAPIError extracts Twilio's JSON error envelope; a non-JSON body (HTML
// error page, empty body) yields a nil envelope and classification falls
// back to the HTTP status alone.
func parseAPIError(body []byte) *apiError {
	if len(body) == 0 {
		return nil
	}
	var ae apiError
	if err := json.Unmarshal(body, &ae); err != nil {
		return nil
	}
	return &ae
}

// parseRetryAfter reads a bounded Retry-After seconds hint. A missing,
// malformed, or over-large hint reports not-ok so the jittered backoff is
// used instead.
func parseRetryAfter(header string) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(header)
	if err != nil || secs <= 0 {
		return 0, false
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d, true
}

// sleepBackoff waits between retry attempts: exponential in the attempt
// number with full jitter, capped, honoring a bounded Retry-After hint when
// present. ctx cancellation aborts the wait.
func sleepBackoff(ctx context.Context, base time.Duration, attempt int, retryAfter time.Duration, retryAfterOK bool) error {
	d := base * time.Duration(1<<attempt)
	d += time.Duration(randIntn(int64(base) + 1)) // full jitter on top
	if d > maxBackoffDelay {
		d = maxBackoffDelay
	}
	if retryAfterOK {
		d = retryAfter
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// randIntn returns a cryptographically-seeded non-negative int64 < n (0 when
// the source fails — a degenerate but safe backoff).
func randIntn(n int64) int64 {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return v.Int64()
}

// defaultSigner is the package-standard event signature: HMAC-SHA256 over
// the body keyed by the auth token ("sha256=<hex>"). The webhook gateway
// verifies signatures fail-closed; the conformance kit asserts presence and
// stability only.
func defaultSigner(key string) comms.Signer {
	return func(body []byte) string {
		mac := sha256.New()
		mac.Write([]byte(key))
		mac.Write([]byte{0x00})
		mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
}

// classify maps an HTTP status + Twilio error envelope onto the application
// error taxonomy (pkg/errors). Twilio's code ranges are classified first
// (they carry more semantics than the bare status); the HTTP status is the
// fallback. Kinds and codes are stable machine identity — callers and the
// core's error mapping rely on them.
//
//	20001-20005  authentication          -> unauthorized
//	20006-20099  permissions             -> forbidden
//	20404        resource not found      -> not_found
//	20429 / 429  rate limited            -> rate_limited
//	21200-21999  input validation        -> invalid
//	30000-39999  call state failures     -> conflict
//	50000-69999  provider-side failures  -> internal (6xxxx included per issue #25)
func classify(httpStatus int, apiErr *apiError) *apperrors.Error {
	kind, code := mapTwilioError(httpStatus, apiErr)
	msg := "twilio request failed"
	if apiErr != nil && strings.TrimSpace(apiErr.Message) != "" {
		msg = strings.TrimSpace(apiErr.Message)
	}
	ae := apperrors.New(kind, code, msg)
	if apiErr != nil {
		ae = ae.WithDetails(map[string]any{
			"twilio_code": apiErr.Code,
			"http_status": httpStatus,
		})
	} else {
		ae = ae.WithDetails(map[string]any{"http_status": httpStatus})
	}
	return ae
}

// mapTwilioError performs the code-range classification documented on
// classify.
func mapTwilioError(httpStatus int, apiErr *apiError) (apperrors.Kind, string) {
	tc := 0
	if apiErr != nil {
		tc = apiErr.Code
	}
	switch {
	case tc >= 20001 && tc <= 20005:
		return apperrors.KindUnauth, "twilio.auth_failed"
	case tc >= 20006 && tc <= 20099:
		return apperrors.KindForbidden, "twilio.forbidden"
	case tc == 20404:
		return apperrors.KindNotFound, "twilio.resource_not_found"
	case tc == 20429 || httpStatus == http.StatusTooManyRequests:
		return apperrors.KindRateLimited, "twilio.rate_limited"
	case tc >= 21200 && tc <= 21999:
		return apperrors.KindInvalid, "twilio.validation_failed"
	case tc >= 30000 && tc <= 39999:
		return apperrors.KindConflict, "twilio.call_state"
	case tc >= 50000 && tc <= 69999:
		return apperrors.KindInternal, "twilio.provider_error"
	}
	switch httpStatus {
	case http.StatusBadRequest:
		return apperrors.KindInvalid, "twilio.request_invalid"
	case http.StatusUnauthorized:
		return apperrors.KindUnauth, "twilio.auth_failed"
	case http.StatusForbidden:
		return apperrors.KindForbidden, "twilio.forbidden"
	case http.StatusNotFound:
		return apperrors.KindNotFound, "twilio.resource_not_found"
	case http.StatusConflict:
		return apperrors.KindConflict, "twilio.call_state"
	case http.StatusTooManyRequests:
		return apperrors.KindRateLimited, "twilio.rate_limited"
	}
	return apperrors.KindInternal, "twilio.provider_error"
}
