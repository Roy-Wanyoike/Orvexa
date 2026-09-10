package africastalking

import (
	"context"
	"encoding/json"
	"errors"
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
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ProviderName is the provider label stamped on every webhook delivery. It
// MUST match the webhook gateway's registry key for Africa's Talking
// (webhooks.ProviderAfricasTalking) so the allowlist verifier routes the
// delivery correctly.
const ProviderName = "africastalking"

const (
	// DefaultAPIBaseURL is the production Africa's Talking Voice REST root.
	// (Research finding, issue #28: the official AT SDKs place the voice
	// surface on the voice.<domain> host — https://voice.africastalking.com —
	// not on api.africastalking.com/v1/voice. Sandbox deployments use
	// https://voice.sandbox.africastalking.com.)
	DefaultAPIBaseURL = "https://voice.africastalking.com"

	// SandboxAPIBaseURL is the AT sandbox voice root.
	SandboxAPIBaseURL = "https://voice.sandbox.africastalking.com"

	// callPath is the make-call resource on the voice host.
	callPath = "/call"

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

	// atMsgLimit bounds provider error text copied into error details.
	atMsgLimit = 256

	// "None" is AT's sentinel for "no error" in its response envelopes.
	atErrorMessageNone = "None"
)

// ErrUnsupportedOperation is the typed error for in-call operations the
// wired Africa's Talking surface cannot perform (e.g. acting on a call leg
// that has already terminated — AT exposes no API for terminated sessions).
// Callers classify it with errors.Is; the wrapped *apperrors.Error keeps the
// client-safe surface (kind conflict, stable machine code).
var ErrUnsupportedOperation = apperrors.Conflict("at.unsupported_operation",
	"operation unsupported by the Africa's Talking voice surface")

// Config is the adapter configuration. Credential fields are registry.Secret
// values (zero-leakage renderings); non-credential endpoint fields are plain
// strings. Build from registry.TelephonyConfig at wiring time:
//
//	p, err := africastalking.New(africastalking.Config{
//	        Username: tc.ATUsername,
//	        APIKey:   tc.ATAPIKey,
//	        Ingest:   ingest,   // comms.IngestFunc
//	        Signer:   signer,   // comms.Signer
//	})
type Config struct {
	// Username is the Africa's Talking account username sent with every
	// make-call request. Credential material — rendered redacted everywhere.
	Username registry.Secret

	// APIKey is the Africa's Talking API key sent as the apikey header.
	// Credential material — rendered redacted everywhere.
	APIKey registry.Secret

	// APIBaseURL overrides the AT Voice REST root (tests point it at a fake
	// server; sandbox deployments point it at SandboxAPIBaseURL). Empty =
	// production default.
	APIBaseURL string

	// Ingest is the delivery hook for translated status callbacks: the same
	// comms.IngestFunc port the built-in Simulator uses (production wiring:
	// the webhook gateway hands verified callback bodies to
	// HandleStatusCallback, which delivers through this port; conformance:
	// the kit's Recorder). Required — lifecycle events must never be
	// silently droppable.
	Ingest comms.IngestFunc `json:"-"`

	// Signer signs the translated webhook body before delivery through
	// Ingest (parity with comms.NewSimulator). Required iff Ingest is set:
	// the webhook gateway is fail-closed and drops unsigned traffic, so an
	// unsigned delivery is behavior that cannot survive production.
	Signer comms.Signer `json:"-"`

	// HTTPClient overrides the default client (10s timeout). Optional.
	HTTPClient *http.Client `json:"-"`

	// MaxRetries bounds retries on 409/429/5xx responses after the initial
	// attempt. Zero value = 3 (the platform default, the "bounded retry ≤3"
	// posture); negative disables retries; values above 3 are clamped to the
	// ceiling. Other 4xx responses and transport errors are terminal — the
	// core's provider-failure path owns the retry-after-failure story.
	MaxRetries int

	// RetryBackoff is the base backoff for exponential retry delays
	// (default 250ms). Each retry's ceiling doubles, and the actual sleep is
	// full-jittered into [0, ceiling) so concurrent senders do not
	// synchronize. Explicit Retry-After guidance from AT is honored exactly
	// (clamped to RetryMaxBackoff) instead of jittered.
	RetryBackoff time.Duration

	// RetryMaxBackoff caps every retry sleep (default 2s).
	RetryMaxBackoff time.Duration

	// Logger receives structured dial/retry logs. Phone numbers and
	// credential material are never logged. Nil = silent.
	Logger *slog.Logger `json:"-"`
}

// Adapter implements telephony.VoiceProvider against the Africa's Talking
// Voice REST API. Construct with New; the zero value is not usable.
type Adapter struct {
	username registry.Secret
	apiKey   registry.Secret
	apiBase  string
	ingest   comms.IngestFunc
	signer   comms.Signer
	http     *http.Client
	log      *slog.Logger

	maxRetries int
	retryBase  time.Duration
	retryCeil  time.Duration
	sleep      func(time.Duration)               // seam for tests
	jitterFn   func(time.Duration) time.Duration // seam for tests: full-jitter pick in [0, d)

	// legs is the live-call registry keyed by the platform interaction id
	// (telephony.ProviderRef convention: the interaction id doubles as the
	// provider leg reference). sessions indexes AT sessionIds back to
	// interaction ids so status callbacks can be routed. Both are
	// in-process state: multi-replica deployments must route an AT callback
	// to the replica holding the session (documented in README.md).
	mu       sync.Mutex
	legs     map[string]*atLeg
	sessions map[string]string
}

// atLeg is one placed call leg tracked by the adapter.
type atLeg struct {
	interactionID string
	tenantID      string
	sessionID     string
	ended         bool           // terminal status observed (platform request or carrier callback)
	held          bool           // platform-side hold state (lifecycle-silent)
	transferred   string         // last transfer destination (audit)
	action        *controlAction // pending carrier-side action for the next callback exchange
}

// New validates the configuration and builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if cfg.Username == "" || cfg.APIKey == "" {
		return nil, apperrors.Invalid("at.credentials_required",
			"ORVEXA_AT_USERNAME and ORVEXA_AT_API_KEY are required")
	}
	if (cfg.Ingest == nil) != (cfg.Signer == nil) {
		return nil, apperrors.Invalid("at.ingest_not_wired",
			"Config.Ingest and Config.Signer must be set together (status callback delivery)")
	}
	if cfg.Ingest == nil {
		return nil, apperrors.Invalid("at.ingest_required",
			"Config.Ingest is required: status callbacks must flow through the signed delivery path")
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
	apiBase := strings.TrimSuffix(cfg.APIBaseURL, "/")
	if apiBase == "" {
		apiBase = DefaultAPIBaseURL
	}
	return &Adapter{
		username:   cfg.Username,
		apiKey:     cfg.APIKey,
		apiBase:    apiBase,
		ingest:     cfg.Ingest,
		signer:     cfg.Signer,
		http:       httpClient,
		log:        cfg.Logger,
		maxRetries: maxRetries,
		retryBase:  retryBase,
		retryCeil:  retryCeil,
		sleep:      time.Sleep,
		jitterFn:   fullJitter,
		legs:       map[string]*atLeg{},
		sessions:   map[string]string{},
	}, nil
}

// String renders the adapter with zero credential material. This exists for
// a subtle reason the redaction tests prove: fmt refuses to invoke
// Stringer/GoStringer on values reached through UNEXPORTED fields, so
// fmt.Sprintf("%v/%+v/%#v", adapter) would print the raw registry.Secret
// values — the adapter must therefore render ITSELF redacted on every
// top-level fmt/slog/json path, exactly like registry.TelephonyConfig.
func (a *Adapter) String() string {
	fields := []string{
		"username=" + a.username.Redacted(),
		"api_key=" + a.apiKey.Redacted(),
	}
	if a.apiBase != "" {
		fields = append(fields, "api_base="+a.apiBase)
	}
	return "africastalking.Adapter(" + strings.Join(fields, " ") + ")"
}

// GoString renders the masked form so %#v cannot disclose credentials.
func (a *Adapter) GoString() string { return a.String() }

// LogValue renders the masked form for structured logging.
func (a *Adapter) LogValue() slog.Value { return slog.StringValue(a.String()) }

// MarshalJSON renders only redacted fields; credential-bearing fields are
// unexported/raw otherwise, so the safe rendering is authoritative.
func (a *Adapter) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"username": a.username.Redacted(),
		"api_key":  a.apiKey.Redacted(),
		"api_base": a.apiBase,
	})
}

// callResponse is the AT make-call envelope: a top-level error message plus
// one entry per dialed number. "errorMessage" of "None" means no error.
type callResponse struct {
	ErrorMessage string      `json:"errorMessage"`
	Entries      []callEntry `json:"entries"`
}

// callEntry is one dialed number's outcome.
type callEntry struct {
	PhoneNumber     string `json:"phoneNumber"`
	SessionID       string `json:"sessionId"`
	Status          string `json:"status"`
	ErrorMessage    string `json:"errorMessage"`
	ClientRequestID string `json:"clientRequestId"`
}

// cleanMsg normalizes an AT envelope message: "None" and empty mean no
// error; everything else is truncated provider text.
func cleanMsg(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == atErrorMessageNone {
		return ""
	}
	return truncate(s, atMsgLimit)
}

// postCall performs the make-call POST under the bounded-retry budget.
// Endpoint/auth details never appear in returned errors.
func (a *Adapter) postCall(ctx context.Context, form url.Values) (*callResponse, error) {
	endpoint := a.apiBase + callPath
	var lastErr *apperrors.Error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, apperrors.Internal("at.request_build_failed", "africastalking request could not be built").WithCause(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		// Research finding (issue #28): AT authenticates the voice surface
		// with the apikey header — the task brief's "Bearer apiKey" does not
		// match the documented wire contract (see README.md).
		req.Header.Set("apikey", string(a.apiKey))

		resp, err := a.http.Do(req)
		if err != nil {
			// Transport errors are NOT retried here: the caller context and
			// the core's provider-failure path own that story. The cause is
			// sanitized so the endpoint cannot balloon into logs.
			return nil, apperrors.Internal("at.transport_error", "africastalking request could not be delivered").
				WithCause(sanitizeTransport(err))
		}
		status := resp.StatusCode
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		_ = resp.Body.Close()

		switch {
		case status >= 200 && status < 300:
			envelope, perr := parseCallResponse(body)
			if perr != nil {
				return nil, perr
			}
			return envelope, nil
		case status == http.StatusConflict:
			lastErr = apperrors.Conflict("at.conflict", "africastalking reported a conflicting request")
		case status == http.StatusTooManyRequests:
			lastErr = apperrors.RateLimited("at.rate_limited", "africastalking rate limited the request")
		case status >= 500:
			lastErr = apperrors.Internal("at.upstream_error", "africastalking request failed")
		default:
			return nil, terminalError(status, parseCallResponseLoose(body))
		}

		if attempt >= a.maxRetries {
			return nil, lastErr.WithDetails(statusDetails(status, parseCallResponseLoose(body)))
		}
		a.pause(ctx, attempt, retryAfter(resp.Header, a.retryCeil))
		if a.log != nil {
			a.log.WarnContext(ctx, "africastalking call retry",
				"attempt", attempt+1, "status", status)
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

// parseCallResponse decodes a 2xx make-call envelope.
func parseCallResponse(body []byte) (*callResponse, error) {
	var env callResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, apperrors.Internal("at.envelope_invalid", "africastalking returned an unreadable response").WithCause(err)
	}
	return &env, nil
}

// parseCallResponseLoose decodes an error envelope best-effort: AT error
// bodies reuse the envelope shape (errorMessage, sometimes errorCode).
func parseCallResponseLoose(body []byte) callResponse {
	var env callResponse
	_ = json.Unmarshal(body, &env)
	return env
}

// terminalError maps a non-retriable status to the application error model.
// Messages never contain the request URL or credentials; provider text is
// bounded and carried as details.
func terminalError(status int, env callResponse) *apperrors.Error {
	var e *apperrors.Error
	switch status {
	case http.StatusUnauthorized:
		e = apperrors.Unauth("at.auth_failed", "africastalking rejected the credentials")
	case http.StatusForbidden:
		e = apperrors.Forbidden("at.forbidden", "africastalking refused the operation")
	case http.StatusNotFound, http.StatusGone:
		e = apperrors.NotFound("at.not_found", "africastalking resource not found")
	default:
		e = apperrors.Invalid("at.request_rejected", "africastalking rejected the request")
	}
	return e.WithDetails(statusDetails(status, env))
}

// statusDetails builds the bounded, credential-free error details map.
func statusDetails(status int, env callResponse) map[string]any {
	d := map[string]any{"http_status": status}
	if msg := cleanMsg(env.ErrorMessage); msg != "" {
		d["at_message"] = msg
	}
	if msg := cleanMsg(entryMessage(env)); msg != "" && msg != cleanMsg(env.ErrorMessage) {
		d["at_entry_message"] = msg
	}
	return d
}

// entryMessage returns the first entry-level error message, if any.
func entryMessage(env callResponse) string {
	for _, e := range env.Entries {
		if m := cleanMsg(e.ErrorMessage); m != "" {
			return m
		}
	}
	return ""
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

// sanitizeTransport rewrites *url.Error so a hostile or broken upstream's
// endpoint rendering cannot balloon through the error chain into logs. The
// AT endpoint carries no credentials (the apikey travels in a header), but
// the bound keeps error lines finite and reviewable.
func sanitizeTransport(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	clone := *ue
	clone.URL = truncate(ue.URL, 128)
	return &clone
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
