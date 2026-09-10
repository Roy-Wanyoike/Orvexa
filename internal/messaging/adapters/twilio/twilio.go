// Package twilio implements the messaging.MessagingProvider port against the
// Twilio Programmable Messaging REST API (SMS first-class; MMS media is
// passed through as MediaUrl parameters without further semantics).
//
// # Send path
//
// Send POSTs form-encoded parameters to
// {APIBaseURL}/2010-04-01/Accounts/{AccountSID}/Messages.json with HTTP basic
// auth (AccountSID as username, AuthToken as password). The StatusCallback
// parameter carries the platform routing keys (interaction_id, tenant_id) so
// Twilio's async status callbacks can be translated back into the
// interaction lifecycle — see TranslateStatus.
//
// # Receipt path
//
// Twilio reports delivery asynchronously: it POSTs form-encoded status
// callbacks to the platform's webhook endpoint. This package provides the
// pure translation helper (TranslateStatus) for the webhook gateway layer,
// plus HandleStatusCallback, the adapter's own in-process delivery path that
// emits signed comms.ProviderEvent webhooks through the same comms.IngestFunc
// port the built-in Simulator uses. Signature VERIFICATION for inbound
// callbacks belongs to the fail-closed webhook gateway
// (internal/webhooks.VerifyTwilio) — never in this package.
//
// # Error taxonomy
//
// Every failure is an *apperrors.Error carrying a stable machine code
// (twilio.*); errors never embed credentials, the Authorization header, or
// the account SID. 429/5xx failures are retried with bounded, jittered
// backoff (see Config.MaxRetries); other failures are terminal.
//
// # Credentials
//
// Credential fields use the registry.Secret type so every rendering path
// (fmt verbs, log/slog, encoding/json) exposes only redacted output.
package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ProviderName is the provider label stamped on every webhook delivery. It
// MUST match the webhook gateway's registry key for Twilio so the signature
// verifier routes the delivery correctly.
const ProviderName = "twilio"

const (
	// defaultAPIBase is the production Twilio REST API root.
	defaultAPIBase = "https://api.twilio.com"

	// messagesPath is the versioned message resource; the account SID is
	// substituted in. The SID never appears in error messages.
	messagesPath = "/2010-04-01/Accounts/%s/Messages.json"

	// QueryInteractionID / QueryTenantID are the routing keys the adapter
	// appends to the StatusCallback URL. Twilio does not echo them in the
	// POST body — they arrive as URL query parameters on the callback, so
	// callers of TranslateStatus merge them into the form (see
	// MergeCallbackForm).
	QueryInteractionID = "interaction_id"
	QueryTenantID      = "tenant_id"

	// maxRetriesCeiling is the hard bound on retry attempts ("bounded retry
	// ≤3"): configurations above it are clamped down.
	maxRetriesCeiling = 3

	defaultMaxRetries   = 3
	defaultRetryBase    = 250 * time.Millisecond
	defaultRetryCeiling = 2 * time.Second
	defaultHTTPTimeout  = 10 * time.Second

	// errBodyLimit bounds how much of an error response body is read; a
	// hostile or broken upstream cannot balloon adapter memory.
	errBodyLimit = 8 << 10

	// twilioMsgLimit bounds provider error text copied into error details.
	twilioMsgLimit = 256
)

// Config is the adapter configuration. Credential fields are registry.Secret
// values (zero-leakage renderings); non-credential endpoint fields are plain
// strings. Build from registry.MessagingConfig at wiring time:
//
//	p, err := twilio.New(twilio.Config{
//	        AccountSID: mc.TwilioAccountSID,
//	        AuthToken:  mc.TwilioAuthToken,
//	        FromNumber: mc.TwilioFromNumber,
//	        ...
//	})
type Config struct {
	// AccountSID is the Twilio account SID (credential material).
	AccountSID registry.Secret

	// AuthToken is the Twilio auth token (credential material, basic-auth
	// password).
	AuthToken registry.Secret

	// FromNumber is the default sending number (E.164 or shortcode). A
	// per-message Message.From overrides it. Either one must be present or
	// Send fails.
	FromNumber registry.Secret

	// APIBaseURL overrides the Twilio REST API root (tests point it at a
	// fake server). Empty = production default.
	APIBaseURL string

	// StatusCallbackBase is the externally routable base URL of the
	// platform's Twilio status-callback webhook endpoint. The adapter appends
	// ?interaction_id=...&tenant_id=... to every send. Empty = no
	// StatusCallback parameter: sends still succeed, but Twilio never calls
	// back and no receipt lifecycle can ever be observed (documented
	// fire-and-forget posture — receipts are the platform's lifecycle truth,
	// so production wiring should set this).
	StatusCallbackBase string

	// Ingest is the delivery hook for translated status callbacks: the same
	// comms.IngestFunc port the built-in Simulator uses (in-process/test
	// wiring; production callbacks enter through the webhook gateway, which
	// verifies Twilio signatures before translating).
	Ingest comms.IngestFunc `json:"-"`

	// Signer signs the translated webhook body before delivery through
	// Ingest (parity with comms.NewSimulator). Required iff Ingest is set.
	Signer comms.Signer `json:"-"`

	// HTTPClient overrides the default client (10s timeout). Optional.
	HTTPClient *http.Client `json:"-"`

	// MaxRetries bounds retries on 429/5xx responses after the initial
	// attempt. Zero value = 3 (the platform default, the "bounded retry ≤3"
	// posture); negative disables retries; values above 3 are clamped to the
	// ceiling. Only 429 and 5xx responses are retried — 4xx is terminal, and
	// transport errors surface immediately (the messaging core's own
	// duplicate-safe retry covers transport flakiness).
	MaxRetries int

	// RetryBackoff is the base backoff for exponential retry delays
	// (default 250ms). Each retry's ceiling doubles (base, 2×base, 4×base,
	// ...) capped at RetryMaxBackoff, and the actual sleep is full-jittered
	// into [0, ceiling) so concurrent senders do not synchronize. Explicit
	// Retry-After guidance from Twilio is honored exactly (clamped to
	// RetryMaxBackoff) instead of jittered.
	RetryBackoff time.Duration

	// RetryMaxBackoff caps every retry sleep (default 2s).
	RetryMaxBackoff time.Duration

	// Logger receives structured send/retry logs. Phone numbers, message
	// bodies and credential material are never logged. Nil = silent.
	Logger *slog.Logger `json:"-"`
}

// Adapter implements messaging.MessagingProvider against the Twilio REST API.
// Construct with New.
type Adapter struct {
	accountSID registry.Secret
	authToken  registry.Secret
	fromNumber registry.Secret
	apiBase    string
	statusBase string
	ingest     comms.IngestFunc
	signer     comms.Signer
	http       *http.Client
	log        *slog.Logger

	maxRetries int
	retryBase  time.Duration
	retryCeil  time.Duration
	sleep      func(time.Duration)               // seam for tests
	jitterFn   func(time.Duration) time.Duration // seam for tests: full-jitter pick in [0, d)

	// sentIDs is the in-process idempotency guard (Simulator semantics):
	// a repeated InteractionID never errors, never re-bills. Claimed before
	// the POST, released only when every attempt failed so a genuine retry
	// after a hard provider outage can proceed.
	sentMu  sync.Mutex
	sentIDs map[string]struct{}
}

// New validates the configuration and builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if cfg.AccountSID == "" || cfg.AuthToken == "" {
		return nil, apperrors.Invalid("twilio.credentials_required",
			"ORVEXA_TWILIO_ACCOUNT_SID and ORVEXA_TWILIO_AUTH_TOKEN are required")
	}
	if (cfg.Ingest == nil) != (cfg.Signer == nil) {
		return nil, apperrors.Invalid("twilio.ingest_not_wired",
			"Config.Ingest and Config.Signer must be set together (status callback delivery)")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	retryBase := cfg.RetryBackoff
	if retryBase <= 0 {
		retryBase = defaultRetryBase
	}
	retryCeil := cfg.RetryMaxBackoff
	if retryCeil <= 0 {
		retryCeil = defaultRetryCeiling
	}
	if retryCeil < retryBase {
		retryCeil = retryBase
	}
	maxRetries := defaultMaxRetries
	switch {
	case cfg.MaxRetries < 0:
		maxRetries = 0
	case cfg.MaxRetries > maxRetriesCeiling:
		maxRetries = maxRetriesCeiling
	case cfg.MaxRetries > 0:
		maxRetries = cfg.MaxRetries
	}
	return &Adapter{
		accountSID: cfg.AccountSID,
		authToken:  cfg.AuthToken,
		fromNumber: cfg.FromNumber,
		apiBase:    strings.TrimSuffix(cfg.APIBaseURL, "/"),
		statusBase: cfg.StatusCallbackBase,
		ingest:     cfg.Ingest,
		signer:     cfg.Signer,
		http:       httpClient,
		log:        cfg.Logger,
		maxRetries: maxRetries,
		retryBase:  retryBase,
		retryCeil:  retryCeil,
		sleep:      time.Sleep,
		jitterFn:   fullJitter,
		sentIDs:    map[string]struct{}{},
	}, nil
}

// Compile-time proof the adapter satisfies the port.
var _ messaging.MessagingProvider = (*Adapter)(nil)

// String renders the configuration with zero credential material. It is the
// deliberate counterpart to Adapter.String: fmt invokes the Stringer for
// %v/%s on the config VALUE, so every top-level rendering of Config — fmt
// verbs, log/slog payloads, panic dumps — is this redacted form. Unset
// fields are omitted; secrets render only in their sanctioned masked forms
// (**** or ****last4). The redaction test pins cfg.String() directly.
func (c Config) String() string {
	fields := make([]string, 0, 6)
	for _, kv := range []struct{ key, val string }{
		{"account_sid", c.AccountSID.Redacted()},
		{"auth_token", c.AuthToken.Redacted()},
		{"from_number", c.FromNumber.Redacted()},
	} {
		if kv.val != "" {
			fields = append(fields, kv.key+"="+kv.val)
		}
	}
	if c.APIBaseURL != "" {
		fields = append(fields, "api_base_url="+c.APIBaseURL)
	}
	if c.StatusCallbackBase != "" {
		fields = append(fields, "status_callback_base="+c.StatusCallbackBase)
	}
	if c.MaxRetries != 0 {
		fields = append(fields, "max_retries="+strconv.Itoa(c.MaxRetries))
	}
	return "twilio.Config(" + strings.Join(fields, " ") + ")"
}

// String renders the adapter with zero credential material. This exists for
// a subtle reason the redaction tests proved: fmt refuses to invoke
// Stringer/GoStringer on values reached through UNEXPORTED fields, so
// fmt.Sprintf("%v/%+v/%#v", adapter) would print the raw registry.Secret
// values — the adapter must therefore render ITSELF redacted on every
// top-level fmt/slog/json path, exactly like registry.MessagingConfig.
func (a *Adapter) String() string {
	fields := []string{
		"account_sid=" + a.accountSID.Redacted(),
		"auth_token=" + a.authToken.Redacted(),
		"from_number=" + a.fromNumber.Redacted(),
	}
	if a.apiBase != "" {
		fields = append(fields, "api_base="+a.apiBase)
	}
	if a.statusBase != "" {
		fields = append(fields, "status_callback_base="+a.statusBase)
	}
	return "twilio.Adapter(" + strings.Join(fields, " ") + ")"
}

// GoString renders the masked form so %#v cannot disclose credentials.
func (a *Adapter) GoString() string { return a.String() }

// LogValue renders the masked form for structured logging.
func (a *Adapter) LogValue() slog.Value { return slog.StringValue(a.String()) }

// MarshalJSON renders only redacted fields; credential-bearing fields would
// be unexported/raw otherwise, so the safe rendering is authoritative.
func (a *Adapter) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"account_sid": a.accountSID.Redacted(),
		"auth_token":  a.authToken.Redacted(),
		"from_number": a.fromNumber.Redacted(),
		"api_base":    a.apiBase,
	})
}

// Send implements messaging.MessagingProvider: one POST to /Messages.json
// carrying From/To/Body (or MediaUrl for media messages) and the
// StatusCallback URL carrying the interaction id.
//
// The core gate (messaging.ValidateMessage) is re-applied so adapter-level
// rejections speak the exact platform taxonomy, then tenant context is
// required (receipts must be tenant-scoped or the lifecycle processor cannot
// route them). Duplicate InteractionIDs are a silent no-op — the core may
// re-invoke Send after transport timeouts, and a second real carrier POST
// would double-bill the customer (see Config docs and README).
func (a *Adapter) Send(ctx context.Context, msg messaging.Message) error {
	if err := messaging.ValidateMessage(&msg); err != nil {
		return err
	}
	if msg.TenantID == "" {
		return apperrors.Invalid("comms.tenant_required", "tenant context is required")
	}

	from := strings.TrimSpace(msg.From)
	if from == "" {
		from = strings.TrimSpace(string(a.fromNumber))
	}
	if from == "" {
		return apperrors.Invalid("twilio.from_required",
			"from is required (message From or ORVEXA_TWILIO_FROM_NUMBER)")
	}
	fromN, err := normalizePhone(from)
	if err != nil {
		return err
	}
	toN, err := normalizePhone(msg.To)
	if err != nil {
		return err
	}
	for _, mu := range msg.MediaURLs {
		if u, perr := url.Parse(mu); perr != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return apperrors.Invalid("twilio.invalid_media", "media URLs must be http(s)")
		}
	}

	form := url.Values{}
	form.Set("From", fromN)
	form.Set("To", toN)
	if strings.TrimSpace(msg.Body) != "" {
		form.Set("Body", msg.Body)
	}
	for _, mu := range msg.MediaURLs {
		form.Add("MediaUrl", mu)
	}
	if a.statusBase != "" {
		cb, err := buildStatusCallback(a.statusBase, msg.TenantID, msg.InteractionID)
		if err != nil {
			return err
		}
		form.Set("StatusCallback", cb)
	}

	if !a.claimInteraction(msg.InteractionID) {
		// Simulator semantics: a repeated InteractionID is a harmless retry
		// or a duplicate dispatch — never an error, never a second bill.
		// Receipt suppression downstream is the processor's same-state
		// no-op (comms.Processor).
		return nil
	}
	if err := a.postMessage(ctx, msg.InteractionID, form); err != nil {
		a.releaseInteraction(msg.InteractionID)
		return err
	}
	return nil
}

// claimInteraction records the interaction id as in-flight-or-sent and
// reports whether THIS caller owns the send (false = duplicate).
func (a *Adapter) claimInteraction(id string) bool {
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	if _, dup := a.sentIDs[id]; dup {
		return false
	}
	a.sentIDs[id] = struct{}{}
	return true
}

// releaseInteraction drops the claim after a failed send so the next call is
// a genuine retry, not a suppressed duplicate.
func (a *Adapter) releaseInteraction(id string) {
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	delete(a.sentIDs, id)
}

// postMessage performs the bounded-retry POST loop. Endpoint/auth details
// never appear in returned errors.
func (a *Adapter) postMessage(ctx context.Context, interactionID string, form url.Values) error {
	endpoint := a.apiBase + fmt.Sprintf(messagesPath, url.PathEscape(string(a.accountSID)))
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return apperrors.Internal("twilio.request_build_failed", "twilio request could not be built").WithCause(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.SetBasicAuth(string(a.accountSID), string(a.authToken))

		resp, err := a.http.Do(req)
		if err != nil {
			// Transport errors are NOT retried here: the messaging core
			// retries provider.Send after transport timeouts under its own
			// duplicate-safe contract (conformance kit, retry-safety).
			// The cause is sanitized so the account SID in the URL can never
			// reach a log line.
			return apperrors.Internal("twilio.transport_error", "twilio request could not be delivered").
				WithCause(sanitizeTransport(err))
		}
		status := resp.StatusCode
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		_ = resp.Body.Close()
		tw := parseTwilioError(body)

		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusTooManyRequests:
			lastErr = apperrors.RateLimited("twilio.rate_limited", "twilio rate limited the request")
			if attempt >= a.maxRetries {
				return lastErr
			}
			a.pause(ctx, attempt, retryAfter(resp.Header, a.retryCeil))
		case status >= 500:
			lastErr = apperrors.Internal("twilio.upstream_error", "twilio request failed")
			if attempt >= a.maxRetries {
				return lastErr
			}
			a.pause(ctx, attempt, retryAfter(resp.Header, a.retryCeil))
		default:
			return terminalError(status, tw)
		}
		if a.log != nil {
			a.log.WarnContext(ctx, "twilio send retry",
				"interaction_id", interactionID, "attempt", attempt+1, "status", status)
		}
	}
}

// pause sleeps before the next retry attempt. Server-provided Retry-After
// guidance (already clamped) is honored exactly; otherwise the delay is
// full-jittered under the exponential ceiling.
func (a *Adapter) pause(ctx context.Context, attempt int, serverDelay time.Duration) {
	if err := ctx.Err(); err != nil {
		return // next loop iteration returns the ctx error without dialing
	}
	delay := serverDelay
	if delay <= 0 {
		ceil := a.retryBase << attempt
		if ceil > a.retryCeil {
			ceil = a.retryCeil
		}
		delay = a.jitterFn(ceil)
	}
	a.sleep(delay)
}

// fullJitter picks a uniform random duration in [0, d) (AWS "full jitter");
// d <= 0 yields 0.
func fullJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d)))
}

// normalizePhone canonicalizes a phone number to E.164 by reusing the
// platform's canonical identifier normalizer (internal/customers), so the
// adapter and the customer identity layer can never disagree about what a
// phone number is. The wrapped error keeps kind invalid under an
// adapter-local code.
func normalizePhone(v string) (string, error) {
	n, err := customers.NormalizeIdentifier(customers.IdentPhone, v)
	if err != nil {
		return "", apperrors.Wrap(err, apperrors.KindInvalid, "twilio.invalid_phone",
			"phone number must be 7-15 digits (E.164)")
	}
	return n, nil
}

// buildStatusCallback appends the platform routing keys to the configured
// callback base. The interaction id travels on the callback URL so Twilio's
// async status callbacks can be routed back to the originating interaction.
func buildStatusCallback(base, tenantID, interactionID string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", apperrors.Invalid("twilio.invalid_status_callback_base",
			"StatusCallbackBase must be an absolute http(s) URL")
	}
	q := u.Query()
	q.Set(QueryInteractionID, interactionID)
	q.Set(QueryTenantID, tenantID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// twilioError is the documented Twilio REST error payload.
type twilioError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func parseTwilioError(body []byte) twilioError {
	if len(body) == 0 {
		return twilioError{}
	}
	var tw twilioError
	if err := json.Unmarshal(body, &tw); err != nil {
		return twilioError{}
	}
	tw.Message = truncate(tw.Message, twilioMsgLimit)
	return tw
}

// terminalError maps a non-retriable status to the application error model.
// Messages never contain the request URL or credentials; provider text is
// bounded and carried as details.
func terminalError(status int, tw twilioError) *apperrors.Error {
	var e *apperrors.Error
	switch status {
	case http.StatusUnauthorized:
		e = apperrors.Unauth("twilio.auth_failed", "twilio rejected the credentials")
	case http.StatusForbidden:
		e = apperrors.Forbidden("twilio.forbidden", "twilio refused the operation")
	case http.StatusNotFound, http.StatusGone:
		e = apperrors.NotFound("twilio.not_found", "twilio resource not found")
	default:
		e = apperrors.Invalid("twilio.request_rejected", "twilio rejected the request")
	}
	return e.WithDetails(twilioDetails(status, tw))
}

func twilioDetails(status int, tw twilioError) map[string]any {
	d := map[string]any{"http_status": status}
	if tw.Code != 0 {
		d["twilio_code"] = tw.Code
	}
	if tw.Message != "" {
		d["twilio_message"] = tw.Message
	}
	return d
}

// retryAfter parses the Retry-After header (integer seconds) and clamps it
// to the configured ceiling. Absent or malformed guidance yields 0.
func retryAfter(h http.Header, ceil time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > ceil {
		d = ceil
	}
	return d
}

// sanitizeTransport rewrites *url.Error so the request URL (which embeds the
// account SID) can never travel through an error chain into a log line.
func sanitizeTransport(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	clone := *ue
	clone.URL = redactEndpoint(ue.URL)
	return &clone
}

func redactEndpoint(u string) string {
	marker := "/Accounts/"
	i := strings.Index(u, marker)
	if i < 0 {
		return u
	}
	rest := u[i+len(marker):]
	j := strings.Index(rest, "/")
	if j < 0 {
		return u[:i+len(marker)] + "[redacted]"
	}
	return u[:i+len(marker)] + "[redacted]" + rest[j:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
