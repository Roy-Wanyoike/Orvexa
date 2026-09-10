// Package africastalking implements Orvexa's Africa's Talking adapter: the
// messaging.MessagingProvider port for SMS (POST /version1/messaging with
// out-of-band delivery reports) plus a minimal inbound USSD session surface
// (TranslateUssdCallback). See the package README for the pinned wire
// contract, the documented carrier limits, and the divergences from the
// issue's shorthand (the carrier's real auth header and API path).
//
// The adapter is honest by design:
//
//   - Credentials arrive as registry.Secret values; every rendering path
//     (fmt verbs, log/slog, encoding/json) exposes only redacted output and
//     the API key travels exclusively in the carrier's "apiKey" request
//     header — never in bodies, errors, logs or tests.
//   - Lifecycle receipts are delivered as signed comms.ProviderEvent
//     webhooks through the injected comms.IngestFunc hook — the exact path
//     the built-in Simulator uses; no provider bypasses the fail-closed
//     webhook gateway.
//   - Send idempotency mirrors the Simulator's contract (a repeated
//     InteractionID never errors) while avoiding silent double billing: a
//     send the carrier already accepted is never re-POSTed.
//   - Transient carrier failures (429/5xx responses, transport errors) are
//     retried with a bounded budget; other failures surface immediately.
//   - The destination address is canonicalized with the platform's own
//     identifier normalizer (customers.NormalizeIdentifier) and must land
//     on strict E.164 before the carrier sees it — one phone taxonomy
//     across CRM identity and carrier addressing.
package africastalking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// DefaultBaseURL is Africa's Talking's production version-1 API root. The
// adapter appends the carrier's published version-1 paths ("messaging");
// see the README for the exact wire contract the fake server tests pin.
const DefaultBaseURL = "https://api.africastalking.com/version1"

// providerLabel is stamped on every delivery this adapter emits. It matches
// registry.ProviderAfricasTalking so the webhook gateway routes translated
// events to the Africa's Talking verification path (peer allowlist).
const providerLabel = string(registry.ProviderAfricasTalking)

const (
	defaultMaxRetries = 2 // retries AFTER the first attempt: 3 requests total
	defaultBaseDelay  = 25 * time.Millisecond
	defaultMaxDelay   = 2 * time.Second
	maxResponseBytes  = 1 << 20 // carrier responses are small; bound the read anyway

	// sentTableCap bounds the in-memory idempotency table. See sendTable:
	// durable dedup lives upstream; eviction has documented consequences.
	sentTableCap = 65536
)

// e164Re is the strict ITU-T E.164 subscriber-number shape the carrier
// boundary demands: '+' then country code (no leading zero) and up to 15
// digits total. It is deliberately stricter than the CRM identifier rule
// (customers.NormalizeIdentifier accepts local formats): that function
// canonicalizes, this one is the operator policy that refuses anything the
// carrier cannot route internationally.
var e164Re = regexp.MustCompile(`^\+[1-9]\d{7,14}$`)

// Config wires the adapter. Credentials are registry.Secret values so no
// rendering path can leak them. Username, APIKey and SenderID are required —
// the registry enforces them at boot (ORVEXA_AT_USERNAME / ORVEXA_AT_API_KEY
// / ORVEXA_AT_SENDER_ID) and the constructor re-checks defensively, listing
// environment variable NAMES only.
type Config struct {
	// Username is the Africa's Talking account username (ORVEXA_AT_USERNAME).
	Username registry.Secret

	// APIKey is the account API key. It travels only in the request's
	// "apiKey" header — never in the body, errors or logs.
	APIKey registry.Secret

	// SenderID is the pre-registered sender ID used when a message carries
	// no per-message From override (ORVEXA_AT_SENDER_ID). Africa's Talking
	// delivers from pre-registered sender IDs or approved shortcodes; the
	// registry validates this credential at boot (fail-closed posture).
	SenderID registry.Secret

	// BaseURL is the carrier API root. Defaults to DefaultBaseURL; tests
	// point it at a fake server.
	BaseURL string

	// Ingest is the signed-webhook delivery hook (comms.IngestFunc).
	// Required: translated lifecycle events flow through it exactly like the
	// Simulator's — in production it POSTs to the platform's own webhook
	// gateway, so no provider bypasses signature validation.
	Ingest comms.IngestFunc

	// Signer signs emitted webhook bodies (comms.Signer). Required: the
	// gateway is fail-closed and drops unsigned deliveries.
	Signer comms.Signer

	// HTTP is the outbound client. Defaults to a client with a sane timeout.
	HTTP *http.Client

	// MaxRetries is the number of retries AFTER the first attempt (default
	// 2). 429/5xx responses and transport errors are retried; other 4xx are
	// not.
	MaxRetries int

	// BaseDelay/MaxDelay bound the exponential retry backoff (defaults
	// 25ms/2s). A carrier Retry-After header (seconds form) is honored but
	// capped at MaxDelay. There is deliberately no jitter: deterministic
	// behavior keeps the fake-server tests exact.
	BaseDelay time.Duration
	MaxDelay  time.Duration

	// TenantID stamps inbound USSD session events (see ussd.go): this
	// adapter serves one Africa's Talking account, so inbound sessions have
	// no per-message tenant context and are stamped with the account's
	// tenant. Outbound SMS receipts carry the message's own TenantID.
	TenantID string

	// UssdHandler answers one inbound USSD callback (menus, flows). Optional:
	// the USSD surface is active only when it is configured (see ussd.go).
	UssdHandler UssdHandler

	// Logger receives structured, non-sensitive operational logs (nil
	// disables logging). Nothing credential-bearing or PII-bearing is ever
	// logged: interaction ids, carrier message ids and event names only.
	Logger *slog.Logger
}

// Adapter implements messaging.MessagingProvider for Africa's Talking SMS
// and adds the minimal inbound USSD session surface
// (TranslateUssdCallback). It is safe for concurrent use.
type Adapter struct {
	cfg      Config
	logger   *slog.Logger
	http     *http.Client
	attempts int
	sends    *sendTable
	sessions *ussdTable
}

// New builds the adapter. It fails fast (apperrors invalid) on missing
// credentials or delivery wiring, listing environment variable names only —
// never values.
func New(cfg Config) (*Adapter, error) {
	var missing []string
	if cfg.Username == "" {
		missing = append(missing, "ORVEXA_AT_USERNAME")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "ORVEXA_AT_API_KEY")
	}
	if cfg.SenderID == "" {
		missing = append(missing, "ORVEXA_AT_SENDER_ID")
	}
	if len(missing) > 0 {
		return nil, apperrors.Invalid("at.missing_credentials",
			"Africa's Talking adapter is missing required credential variable(s): "+strings.Join(missing, ", "))
	}
	if cfg.Ingest == nil {
		return nil, apperrors.Invalid("at.ingest_required", "the signed-webhook delivery hook (Ingest) is required")
	}
	if cfg.Signer == nil {
		return nil, apperrors.Invalid("at.signer_required", "the webhook body signer (Signer) is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = defaultBaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultMaxDelay
	}
	if cfg.MaxRetries <= 0 {
		// The zero value selects the documented default budget (Config doc):
		// an adapter that never retries would fail real interactions on
		// harmless 429s/transport blips. There is deliberately no
		// MaxRetries=0 escape hatch — the budget is bounded either way.
		cfg.MaxRetries = defaultMaxRetries
	}
	attempts := cfg.MaxRetries + 1
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Adapter{
		cfg:      cfg,
		logger:   logger,
		http:     cfg.HTTP,
		attempts: attempts,
		sends:    newSendTable(sentTableCap),
		sessions: newUssdTable(sentTableCap),
	}, nil
}

// prepared is a fully validated outbound message: every field the carrier
// request needs, resolved once before the idempotency reservation so a
// rejected message can never touch the table or the wire.
type prepared struct {
	interactionID string
	tenantID      string
	from          string
	to            string
	text          string
}

// Send implements messaging.MessagingProvider (SMS). The adapter enforces
// the tenant context, re-validates the message through the core gate
// (messaging.ValidateMessage — one taxonomy across the plane), canonicalizes
// the destination to strict E.164, maps the sender ID (a per-message From
// overrides the registry SenderID), POSTs the carrier's messaging endpoint
// and — on acceptance — emits the signed message.sent receipt synchronously.
// Networks that confirm delivery inline (recipient statusCode 102) also get
// message.delivered here; all other deliveries arrive via the delivery-report
// callback (TranslateDelivery).
//
// Idempotency (Simulator semantics): a repeated InteractionID never errors.
// Unlike the reference re-emit behavior, a send the carrier already accepted
// is NOT re-POSTed — silent double billing is worse than a suppressed
// receipt — and the downstream processor's same-state no-op keeps the
// lifecycle consistent either way (both honest strategies pass the kit).
func (a *Adapter) Send(ctx context.Context, msg messaging.Message) error {
	prep, err := a.prepare(msg)
	if err != nil {
		return err
	}
	for {
		slot, fresh := a.sends.reserve(prep.interactionID)
		if fresh {
			return a.doSend(ctx, prep, slot)
		}
		_, accepted, err := slot.wait(ctx)
		if err != nil {
			return apperrors.Internal("at.transport_failed", "timed out waiting for the in-flight send").WithCause(err)
		}
		if accepted {
			return nil // already accepted by the carrier — suppress, never re-bill
		}
		// The earlier attempt failed and released its reservation; loop to
		// take a fresh one.
	}
}

// prepare validates the message and renders the carrier payload. Order is
// the taxonomy contract: tenant context, interaction id, the shared core
// gate (exact ValidateMessage codes), then the carrier-boundary E.164 rule.
func (a *Adapter) prepare(msg messaging.Message) (*prepared, error) {
	if strings.TrimSpace(msg.TenantID) == "" {
		return nil, apperrors.Invalid("message.tenant_required", "tenant context is required")
	}
	if strings.TrimSpace(msg.InteractionID) == "" {
		return nil, apperrors.Invalid("message.interaction_required", "interaction id is required")
	}
	if err := messaging.ValidateMessage(&msg); err != nil {
		return nil, err
	}
	text, err := smsText(msg)
	if err != nil {
		return nil, err
	}
	to, err := normalizeE164(msg.To)
	if err != nil {
		return nil, err
	}
	from := string(a.cfg.SenderID)
	if f := strings.TrimSpace(msg.From); f != "" {
		from = f
	}
	return &prepared{
		interactionID: msg.InteractionID,
		tenantID:      msg.TenantID,
		from:          from,
		to:            to,
		text:          text,
	}, nil
}

// normalizeE164 canonicalizes a destination the way the platform
// canonicalizes every phone identifier (customers.NormalizeIdentifier — the
// single E.164 taxonomy: trims, drops the 00 prefix, adds '+') and then
// enforces the strict international form the carrier requires. The canonical
// form is what reaches the carrier, never the raw input.
func normalizeE164(to string) (string, error) {
	v, err := customers.NormalizeIdentifier(customers.IdentPhone, to)
	if err != nil {
		return "", apperrors.Invalid("at.to_not_e164", "destination must be a routable international phone number").WithCause(err)
	}
	if !e164Re.MatchString(v) {
		return "", apperrors.Invalid("at.to_not_e164", "destination must be strict E.164 (+<country><subscriber>, 8-15 digits)")
	}
	return v, nil
}

// doSend performs the carrier round-trip for one fresh reservation.
func (a *Adapter) doSend(ctx context.Context, prep *prepared, slot *sendSlot) error {
	status, body, err := a.post(ctx, "/messaging", smsRequest{
		Username: string(a.cfg.Username),
		To:       prep.to,
		Message:  prep.text,
		From:     prep.from,
	})
	if err != nil {
		a.sends.release(prep.interactionID, slot)
		return err
	}
	if status < 200 || status >= 300 {
		a.sends.release(prep.interactionID, slot)
		return mapCarrierError(status)
	}
	var out smsResponse
	if err := json.Unmarshal(body, &out); err != nil || len(out.SMSMessageData.Recipients) == 0 {
		a.sends.release(prep.interactionID, slot)
		return apperrors.Internal("at.malformed_response",
			"carrier accepted the request but returned an unparseable send response").WithCause(err)
	}
	r := out.SMSMessageData.Recipients[0]
	if !carrierAccepted(r.StatusCode, r.Status) {
		a.sends.release(prep.interactionID, slot)
		return apperrors.Internal("at.recipient_rejected", "carrier rejected the recipient").
			WithCause(fmt.Errorf("statusCode=%d status=%q", r.StatusCode, r.Status))
	}
	a.sends.commit(slot, &sendRecord{
		InteractionID: prep.interactionID,
		TenantID:      prep.tenantID,
		MessageID:     r.MessageID,
	})

	// The carrier has the message: emit the sent receipt synchronously. A
	// delivery-hook failure here must NOT fail the send — retry once inline
	// (Simulator semantics), then surface in logs only.
	sent := comms.ProviderEvent{Event: "message.sent", InteractionID: prep.interactionID, TenantID: prep.tenantID}
	if err := a.emitWithRetry(ctx, sent); err != nil {
		a.logger.WarnContext(ctx, "africastalking: sent-receipt delivery failed after carrier acceptance",
			slog.String("interaction_id", prep.interactionID), slog.String("error", err.Error()))
	}
	// Networks that confirm delivery inline (statusCode 102, or the literal
	// "Delivered" status) close the loop immediately — delivered is a real
	// carrier statement here, not a synthesized one. Every other delivery
	// arrives via the delivery-report callback (TranslateDelivery).
	if deliveredInline(r.StatusCode, r.Status) {
		delivered := comms.ProviderEvent{Event: "message.delivered", InteractionID: prep.interactionID, TenantID: prep.tenantID}
		if err := a.emitWithRetry(ctx, delivered); err != nil {
			a.logger.WarnContext(ctx, "africastalking: delivered-receipt delivery failed after inline confirmation",
				slog.String("interaction_id", prep.interactionID), slog.String("error", err.Error()))
		}
	}
	return nil
}

// smsText renders the outbound payload. SMS cannot carry media: when the
// body is empty and media URLs are present, the URLs become the text payload
// (documented divergence — adapters must not fork the lifecycle by content
// type, and the conformance kit exercises media-only messages). The core's
// 4096-rune limit is re-checked after that join: the core validates Body
// alone, and joined URLs could exceed it.
func smsText(msg messaging.Message) (string, error) {
	text := msg.Body
	if strings.TrimSpace(text) == "" && len(msg.MediaURLs) > 0 {
		text = strings.Join(msg.MediaURLs, "\n")
	}
	if strings.TrimSpace(text) == "" {
		return "", apperrors.Invalid("message.body_required", "body or media is required")
	}
	if utf8.RuneCountInString(text) > 4096 {
		return "", apperrors.Invalid("message.body_too_long", "body must be at most 4096 chars")
	}
	return text, nil
}

// smsRequest is the Africa's Talking bulk-SMS request body. The API key is
// NOT part of the body — it travels exclusively in the apiKey header.
type smsRequest struct {
	Username string `json:"username"`
	To       string `json:"to"`
	Message  string `json:"message"`
	From     string `json:"from"`
}

// smsRecipient is one per-recipient entry of the synchronous send response.
type smsRecipient struct {
	StatusCode int    `json:"statusCode"`
	Status     string `json:"status"`
	MessageID  string `json:"messageId"`
}

// smsResponse is the synchronous send response envelope: the carriers
// documented shape is {"SMSMessageData":{"Recipients":[…]}} — the
// Recipients tag must match the wire, or every accepted send parses as
// empty and fails the client (at.malformed_response).
type smsResponse struct {
	SMSMessageData struct {
		Recipients []smsRecipient `json:"Recipients"`
	} `json:"SMSMessageData"`
}

// carrierAccepted reports whether the synchronous per-recipient status means
// the carrier accepted the message for delivery (AT status codes 100
// Processed / 101 Sent / 102 Delivered, or the literal "Success"). Anything
// else is a synchronous rejection and surfaces as a send error — per the
// task contract, message.failed is reserved for delivery-report callbacks.
func carrierAccepted(statusCode int, status string) bool {
	switch statusCode {
	case 100, 101, 102:
		return true
	}
	return strings.EqualFold(strings.TrimSpace(status), "Success")
}

// deliveredInline reports whether the synchronous acceptance already
// confirmed final delivery (AT statusCode 102, or the literal "Delivered"
// status). Only such explicit carrier statements emit message.delivered
// without a delivery-report callback.
func deliveredInline(statusCode int, status string) bool {
	return statusCode == 102 || strings.EqualFold(strings.TrimSpace(status), "Delivered")
}

// mapCarrierError turns a non-2xx carrier response into the application
// error model. Messages are stable and client-safe; only the HTTP status
// travels as the private cause. Neither credential material nor response
// bodies are echoed.
func mapCarrierError(status int) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return apperrors.New(apperrors.KindUnauth, "at.unauthorized", "carrier rejected the credentials").
			WithCause(fmt.Errorf("http status=%d", status))
	case status >= 400 && status < 500:
		return apperrors.Invalid("at.send_rejected", "carrier rejected the request").
			WithCause(fmt.Errorf("http status=%d", status))
	default:
		return apperrors.Internal("at.provider_error", "carrier returned an unexpected response").
			WithCause(fmt.Errorf("http status=%d", status))
	}
}

// post performs one carrier call with the bounded retry budget: 429/5xx
// responses and transport errors are retried with exponential backoff
// (honoring a bounded Retry-After), everything else returns immediately.
func (a *Adapter) post(ctx context.Context, path string, payload any) (int, []byte, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, apperrors.Internal("at.request_encode_failed", "could not encode the carrier request").WithCause(err)
	}
	url := a.cfg.BaseURL + path

	var (
		lastErr      error
		lastRetryAft time.Duration
	)
	for attempt := 0; attempt < a.attempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, a.cfg.BaseDelay, a.cfg.MaxDelay, attempt, lastRetryAft); err != nil {
				return 0, nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			return 0, nil, apperrors.Internal("at.transport_failed", "could not build the carrier request").WithCause(err)
		}
		req.Header.Set("apiKey", string(a.cfg.APIKey))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := a.http.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return 0, nil, apperrors.Internal("at.transport_failed", "carrier request canceled").WithCause(ctxErr)
			}
			lastErr = apperrors.Internal("at.transport_failed", "carrier transport failed").WithCause(err)
			lastRetryAft = 0
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = apperrors.Internal("at.transport_failed", "could not read the carrier response").WithCause(readErr)
			lastRetryAft = 0
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastRetryAft = retryAfter(resp.Header.Get("Retry-After"))
			if resp.StatusCode == http.StatusTooManyRequests {
				lastErr = apperrors.RateLimited("at.rate_limited", "carrier rate limited the request").
					WithCause(fmt.Errorf("http status=%d", resp.StatusCode))
			} else {
				lastErr = apperrors.Internal("at.provider_unavailable", "carrier temporarily unavailable").
					WithCause(fmt.Errorf("http status=%d", resp.StatusCode))
			}
			continue
		}
		return resp.StatusCode, body, nil
	}
	return 0, nil, lastErr
}

// retryAfter parses a Retry-After header in seconds form (the only form AT
// documents); anything unparseable yields 0 and the backoff schedule applies.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// sleepBackoff waits between attempts: the carrier's Retry-After when it
// sent a positive one (capped at max), else exponential backoff
// BaseDelay*2^(attempt-1) capped at max. Cancellation aborts the wait.
func sleepBackoff(ctx context.Context, base, max time.Duration, attempt int, retryAfter time.Duration) error {
	delay := base
	for i := 1; i < attempt && delay < max; i++ {
		delay *= 2
	}
	if delay <= 0 || delay > max {
		delay = max
	}
	if retryAfter > 0 {
		delay = retryAfter
		if delay > max {
			delay = max
		}
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return apperrors.Internal("at.transport_failed", "carrier request canceled during retry backoff").WithCause(ctx.Err())
	case <-t.C:
		return nil
	}
}

// emit delivers one signed ProviderEvent through the ingest hook — the same
// path the built-in Simulator uses. The signature is computed over the body
// by the configured Signer; the webhook gateway verifies it fail-closed.
func (a *Adapter) emit(ctx context.Context, ev comms.ProviderEvent) error {
	body, err := ev.Encode()
	if err != nil {
		return apperrors.Internal("at.event_encode_failed", "could not encode the provider event").WithCause(err)
	}
	return a.cfg.Ingest(ctx, providerLabel, body, a.cfg.Signer(body))
}

// emitWithRetry mirrors the Simulator's receipt semantics: a failed
// delivery-hook attempt is retried once before the failure surfaces to the
// caller (which logs it — the carrier interaction itself is unaffected).
func (a *Adapter) emitWithRetry(ctx context.Context, ev comms.ProviderEvent) error {
	if err := a.emit(ctx, ev); err != nil {
		return a.emit(ctx, ev)
	}
	return nil
}

// deliveryReport is the native Africa's Talking SMS delivery-report
// callback. Only the fields the adapter consumes are decoded; AT sends a
// richer object and unknown fields are ignored. The canonical wire shape is
// AT's documented JSON POST; a tolerant form-encoded fallback (the same
// keys, urlencoded) is accepted because gateways in the wild forward both.
type deliveryReport struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	FailureReason string `json:"failureReason"`
}

// Native delivery-report status classes (the exact mapping lives in
// TranslateDelivery and the README).
const (
	statusSuccess   = "Success"
	statusDelivered = "Delivered"
	statusFailed    = "Failed"
	statusRejected  = "Rejected"
	statusDiscarded = "Discarded"
	statusExpired   = "Expired"
	statusCancelled = "Cancelled"
	statusUndeliv   = "Undelivered"
)

// parseDeliveryReport decodes a delivery report from its content type:
// JSON (AT's documented shape), form-encoded (the same keys urlencoded —
// the fallback some gateways produce), or a sniffed best effort when the
// content type is absent/unknown.
func parseDeliveryReport(contentType string, body []byte) (deliveryReport, error) {
	var dr deliveryReport
	mediaType := strings.TrimSpace(strings.Split(strings.ToLower(contentType), ";")[0])
	parseForm := func() error {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return err
		}
		dr.ID = firstNonEmpty(values.Get("id"), values.Get("messageId"))
		dr.Status = values.Get("status")
		dr.FailureReason = firstNonEmpty(values.Get("failureReason"), values.Get("failure_reason"))
		return nil
	}
	switch mediaType {
	case "application/json":
		if err := json.Unmarshal(body, &dr); err != nil {
			return dr, err
		}
	case "application/x-www-form-urlencoded":
		if err := parseForm(); err != nil {
			return dr, err
		}
	default:
		// Sniff: AT's documented shape is JSON; fall back to form fields.
		if err := json.Unmarshal(body, &dr); err != nil {
			if err := parseForm(); err != nil {
				return dr, err
			}
		}
	}
	return dr, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// TranslateDelivery translates a native AT SMS delivery report into the
// platform's ProviderEvent contract and delivers it through the signed
// ingest hook:
//
//	status Success|Delivered                  -> message.delivered
//	status Failed|Rejected|Discarded|Expired|
//	       Cancelled|Undelivered               -> message.failed (Detail = reason)
//	anything else (intermediate/unknown)       -> acknowledged, no event
//
// The caller (the webhook gateway) must have verified the callback's origin
// — the AT peer-IP allowlist, the only server-side origin control AT
// supports (internal/webhooks.VerifyAfricasTalking) — BEFORE invoking
// translation. The adapter trusts that gate and re-stamps the platform
// signature on the emitted event.
//
// Reports referencing an unknown carrier message id fail as not-found:
// receipts may never materialize state for messages the platform never
// sent. An ingest failure is returned so the gateway can surface it (AT
// retries delivery reports).
func (a *Adapter) TranslateDelivery(ctx context.Context, contentType string, body []byte) error {
	dr, err := parseDeliveryReport(contentType, body)
	if err != nil {
		return apperrors.Invalid("at.malformed_callback", "delivery report is not parseable").WithCause(err)
	}
	if strings.TrimSpace(dr.ID) == "" {
		return apperrors.Invalid("at.malformed_callback", "delivery report carries no message id")
	}
	status := strings.TrimSpace(dr.Status)
	if status == "" {
		return apperrors.Invalid("at.malformed_callback", "delivery report carries no status")
	}
	rec, ok := a.sends.byMessage(dr.ID)
	if !ok {
		return apperrors.NotFound("at.unknown_message_id", "delivery report for an unknown carrier message id")
	}
	switch strings.ToLower(status) {
	case strings.ToLower(statusSuccess), strings.ToLower(statusDelivered):
		return a.emitWithRetry(ctx, comms.ProviderEvent{
			Event:         "message.delivered",
			InteractionID: rec.InteractionID,
			TenantID:      rec.TenantID,
		})
	case strings.ToLower(statusFailed), strings.ToLower(statusRejected),
		strings.ToLower(statusDiscarded), strings.ToLower(statusExpired),
		strings.ToLower(statusCancelled), strings.ToLower(statusUndeliv):
		reason := strings.TrimSpace(dr.FailureReason)
		if reason == "" {
			reason = status
		}
		return a.emitWithRetry(ctx, comms.ProviderEvent{
			Event:         "message.failed",
			InteractionID: rec.InteractionID,
			TenantID:      rec.TenantID,
			Detail:        reason,
		})
	default:
		// Intermediate carrier status (e.g. "Sent"/"Enroute"): acknowledged
		// without a lifecycle event — the send acceptance already emitted
		// message.sent. Unknown statuses are acknowledged too, never guessed:
		// inventing lifecycle transitions from undocumented carrier strings
		// would corrupt conversations.
		a.logger.InfoContext(ctx, "africastalking: intermediate delivery-report status acknowledged",
			slog.String("message_id", dr.ID), slog.String("status", status))
		return nil
	}
}

// sendRecord correlates one accepted send: the platform interaction it
// belongs to, its tenant, and the carrier message id the delivery reports
// key on. No credential or content material is retained.
type sendRecord struct {
	InteractionID string
	TenantID      string
	MessageID     string
}

// sendSlot is the single-flight reservation for one InteractionID. Waiters
// block on done; a non-nil record after close means the carrier accepted.
type sendSlot struct {
	done chan struct{}
	rec  *sendRecord
}

// wait blocks until the owning send completes (or ctx expires) and reports
// whether the carrier accepted. close(done) happens-before the read, so the
// record read is race-free.
func (s *sendSlot) wait(ctx context.Context) (*sendRecord, bool, error) {
	select {
	case <-s.done:
		return s.rec, s.rec != nil, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// sendTable is the adapter's bounded idempotency table: interaction id →
// single-flight slot, plus a carrier message id index for delivery-report
// translation. It is in-memory and per-process; DURABLE dedup lives upstream
// (the webhook gateway's provider-event dedup and the interaction
// lifecycle's same-state no-op). The oldest completed entries are evicted
// beyond the cap — a delivery report for an evicted message id then fails as
// not-found instead of materializing phantom state.
type sendTable struct {
	cap   int
	mu    sync.Mutex
	order []string
	slots map[string]*sendSlot
	byMsg map[string]*sendRecord
}

func newSendTable(cap int) *sendTable {
	if cap <= 0 {
		cap = sentTableCap
	}
	return &sendTable{cap: cap, slots: map[string]*sendSlot{}, byMsg: map[string]*sendRecord{}}
}

// reserve registers a single-flight slot for the interaction and reports
// whether the caller is the fresh owner (false = a send for this interaction
// is already in flight or accepted — wait on the returned slot).
func (t *sendTable) reserve(interactionID string) (*sendSlot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.slots[interactionID]; ok {
		return s, false
	}
	s := &sendSlot{done: make(chan struct{})}
	t.slots[interactionID] = s
	t.order = append(t.order, interactionID)
	t.evictLocked()
	return s, true
}

// commit marks the slot accepted and indexes the carrier message id.
func (t *sendTable) commit(s *sendSlot, rec *sendRecord) {
	t.mu.Lock()
	s.rec = rec
	if rec != nil && rec.MessageID != "" {
		t.byMsg[rec.MessageID] = rec
	}
	t.mu.Unlock()
	close(s.done)
}

// release drops a FAILED reservation so a later retry can send fresh.
func (t *sendTable) release(interactionID string, s *sendSlot) {
	t.mu.Lock()
	if cur := t.slots[interactionID]; cur == s {
		delete(t.slots, interactionID)
	}
	t.mu.Unlock()
	close(s.done)
}

// byMessage resolves a carrier message id to its send record.
func (t *sendTable) byMessage(messageID string) (*sendRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.byMsg[messageID]
	return rec, ok
}

// evictLocked forgets the oldest COMPLETED entries beyond the cap. Reserved
// (in-flight or failed) slots are kept — they are bounded by the request
// lifecycle — and their order entries ride along.
func (t *sendTable) evictLocked() {
	overflow := len(t.order) - t.cap
	if overflow <= 0 {
		return
	}
	keep := make([]string, 0, len(t.order))
	for _, id := range t.order {
		s := t.slots[id]
		if overflow > 0 && (s == nil || slotClosed(s)) {
			if s != nil {
				if s.rec != nil && s.rec.MessageID != "" {
					delete(t.byMsg, s.rec.MessageID)
				}
				delete(t.slots, id)
			}
			overflow--
			continue
		}
		keep = append(keep, id)
	}
	t.order = keep
}

func slotClosed(s *sendSlot) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
