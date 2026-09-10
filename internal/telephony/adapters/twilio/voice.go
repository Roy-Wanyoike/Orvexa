// Voice operations: the telephony.VoiceProvider port implemented over the
// Twilio Programmable Voice REST API (see client.go for the transport).
//
// Operation → REST mapping:
//
//	PlaceCall  POST /Calls.json          To, From, Url (TwiML entry), Method,
//	                                     StatusCallback (self-scoping, see
//	                                     callbackURL), StatusCallbackMethod,
//	                                     StatusCallbackEvent, optional Record
//	Hangup     POST /Calls/{Sid}.json    Status=completed
//	Transfer   POST /Calls/{Sid}.json    Twiml=<Response><Dial><Number>…</Dial></Response>
//	Hold       POST /Calls/{Sid}.json    Twiml=<Response><Pause length="3600"/><Redirect>hold doc</Redirect></Response>
//	Resume     POST /Calls/{Sid}.json    Twiml=<Response><Redirect>resume doc</Redirect></Response>
//
// Leg addressing: Twilio mints the CallSid; the platform addresses legs by
// interaction id (telephony.ProviderRef). The adapter keeps the interaction →
// CallSid map for the process lifetime, so a live leg placed by THIS process
// is fully operable, while references this process never placed fail loudly
// as typed not_found errors (the conformance contract) instead of silently
// no-oping. Restarts lose the map; the self-scoping StatusCallback URLs keep
// event delivery correct across restarts regardless (events.go).
package twilio

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Adapter implements telephony.VoiceProvider over the Twilio REST client.
// Construct with New; the zero value is not usable.
type Adapter struct {
	cfg    Config
	cl     *client
	signer comms.Signer

	mu   sync.Mutex
	legs map[string]*leg
}

// leg is the adapter's per-call bookkeeping: how the platform's interaction
// id maps onto Twilio's CallSid plus the documents this leg was dialled with.
type leg struct {
	callSID     string
	tenantID    string
	twimlURL    string // the entry TwiML document this leg was dialled with
	callbackURL string // the self-scoping status callback URL (events.go)
}

// New builds the Twilio voice adapter. The signer defaults to the package
// HMAC scheme (defaultSigner) so every delivered event is signed the way the
// fail-closed webhook gateway expects. Config validation is per-operation:
// PlaceCall requires the TwiML/callback URLs, Hold requires the hold
// document — failures surface as typed errors naming the missing knob.
func New(cfg Config) *Adapter {
	signer := cfg.Signer
	if signer == nil {
		signer = defaultSigner(string(cfg.AuthToken))
	}
	return &Adapter{
		cfg:    cfg,
		cl:     newClient(&cfg),
		signer: signer,
		legs:   map[string]*leg{},
	}
}

// Compile-time port conformance.
var (
	_ telephony.VoiceProvider = (*Adapter)(nil)
)

// ProviderOptions keys understood by this adapter (validated here, opaque to
// the core).
const (
	// optTwiMLURL overrides Config.TwiMLBaseURL for one call (per-tenant or
	// per-flow IVR entry documents).
	optTwiMLURL = "twilio_url"
	// optRecord turns on Twilio's basic per-call recording flag (issue #25
	// scope: the flag only — no conference/recording APIs).
	optRecord = "record"
)

// e164ish is the documented dial-string contract: an optional leading + and
// 3–15 digits — E.164 for PSTN dialing, short numeric internal extensions
// for on-prem planes. Anything else is rejected before it can reach the
// carrier.
var e164ish = regexp.MustCompile(`^\+?[0-9]{3,15}$`)

// errUnknownLeg is the typed failure for any operation referencing a leg
// this process did not place. A package sentinel keeps repeat failures
// shape-identical (the conformance kit asserts determinism) and the kind is
// not_found: 404 semantics — the resource does not exist here.
var errUnknownLeg = apperrors.NotFound("twilio.unknown_leg", "unknown provider leg reference")

// PlaceCall implements telephony.VoiceProvider: it dials the PSTN leg and
// registers it for callback translation. Validation happens before any
// carrier interaction — a rejected command makes zero REST calls and emits
// zero events. On REST success the leg is registered even if the response
// omits the sid (the call is live either way; unaddressable legs are
// reported by the management operations below).
//
// Twilio is instructed to POST status callbacks to
// CallbackBaseURL?interaction=<id>&tenant=<tenant>: the passthrough makes
// every callback self-scoping, so the events translation (events.go) needs
// no carrier-side lookup. Callbacks fire asynchronously from Twilio — the
// lifecycle events arrive after this call returns.
func (a *Adapter) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	tenantID, _ := cmd.ProviderOptions["tenant_id"].(string)
	if strings.TrimSpace(tenantID) == "" {
		return apperrors.Invalid("twilio.tenant_required",
			"PlaceCall requires tenant context (ProviderOptions tenant_id)")
	}
	if strings.TrimSpace(cmd.InteractionID) == "" {
		return apperrors.Invalid("twilio.interaction_required",
			"PlaceCall requires an interaction id")
	}
	from := strings.TrimSpace(cmd.From)
	to := strings.TrimSpace(cmd.To)
	if !e164ish.MatchString(from) {
		return apperrors.Invalid("twilio.invalid_phone_number",
			"from must be an E.164 number or numeric extension")
	}
	if !e164ish.MatchString(to) {
		return apperrors.Invalid("twilio.invalid_phone_number",
			"to must be an E.164 number or numeric extension")
	}

	twimlURL := strings.TrimSpace(a.cfg.TwiMLBaseURL)
	if v, ok := cmd.ProviderOptions[optTwiMLURL].(string); ok && strings.TrimSpace(v) != "" {
		twimlURL = strings.TrimSpace(v)
	}
	if twimlURL == "" {
		return apperrors.Invalid("twilio.twiml_url_required",
			"PlaceCall requires a TwiML entry document (Config.TwiMLBaseURL or ProviderOptions twilio_url)")
	}
	if strings.TrimSpace(a.cfg.CallbackBaseURL) == "" {
		return apperrors.Invalid("twilio.callback_url_required",
			"PlaceCall requires a status callback endpoint (Config.CallbackBaseURL)")
	}

	form := url.Values{
		"To":                   {to},
		"From":                 {from},
		"Url":                  {twimlURL},
		"Method":               {http.MethodPost},
		"StatusCallback":       {callbackURL(a.cfg.CallbackBaseURL, cmd.InteractionID, tenantID)},
		"StatusCallbackMethod": {http.MethodPost},
		"StatusCallbackEvent":  {"ringing", "answered", "completed"},
	}
	if record, ok := cmd.ProviderOptions[optRecord].(bool); ok && record {
		form.Set("Record", "true")
	}

	cr, err := a.cl.post(ctx, a.callsPath(), form)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.legs[cmd.InteractionID] = &leg{
		callSID:     cr.SID,
		tenantID:    tenantID,
		twimlURL:    twimlURL,
		callbackURL: form.Get("StatusCallback"),
	}
	a.mu.Unlock()
	return nil
}

// Hangup implements telephony.VoiceProvider: the leg is terminated with
// Status=completed and Twilio's completed status callback carries the
// call.ended event through the gateway (events.go).
func (a *Adapter) Hangup(ctx context.Context, providerRef string) error {
	l, ok := a.leg(providerRef)
	if !ok {
		return errUnknownLeg
	}
	if l.callSID == "" {
		return apperrors.Internal("twilio.leg_unaddressable",
			"the provider accepted the call without returning a call sid; the leg cannot be managed")
	}
	_, err := a.cl.post(ctx, a.callPath(l.callSID), url.Values{"Status": {"completed"}})
	return err
}

// Transfer implements telephony.VoiceProvider: the live leg is redirected
// to a fresh <Dial> of the destination. Twilio fires no status callback for
// a TwiML redirect (the leg's CallStatus stays in-progress throughout), so
// the adapter announces the new ringing phase itself — the dial is the
// direct, deterministic result of this REST call, and the destination must
// be observable in the event Detail for the audit trail. A failed REST call
// emits nothing.
func (a *Adapter) Transfer(ctx context.Context, providerRef, destination string) error {
	l, ok := a.leg(providerRef)
	if !ok {
		return errUnknownLeg
	}
	if l.callSID == "" {
		return apperrors.Internal("twilio.leg_unaddressable",
			"the provider accepted the call without returning a call sid; the leg cannot be managed")
	}
	dest := strings.TrimSpace(destination)
	if !e164ish.MatchString(dest) {
		return apperrors.Invalid("twilio.invalid_transfer_destination",
			"transfer destination must be an E.164 number or numeric extension")
	}
	twiml := `<Response><Dial><Number>` + xmlText(dest) + `</Number></Dial></Response>`
	if _, err := a.cl.post(ctx, a.callPath(l.callSID), url.Values{"Twiml": {twiml}}); err != nil {
		return err
	}
	return a.deliver(ctx, &comms.ProviderEvent{
		Event:         "call.ringing",
		InteractionID: providerRef,
		TenantID:      l.tenantID,
		Detail:        "transfer:" + dest,
	})
}

// Hold implements telephony.VoiceProvider by parking the live leg on the
// looping hold document: the leg is redirected to
// <Response><Pause length="3600"/><Redirect method="POST">HOLD</Redirect></Response>
// — a full-hour silent pause, then a redirect back to the same document, so
// the leg stays parked until Resume redirects it away. Twilio has no native
// hold on a live leg; without a configured hold document (Config.HoldTwiMLURL)
// Hold fails with a typed error rather than faking the semantics.
// Lifecycle-silent by contract: no events are emitted.
func (a *Adapter) Hold(ctx context.Context, providerRef string) error {
	l, ok := a.leg(providerRef)
	if !ok {
		return errUnknownLeg
	}
	if l.callSID == "" {
		return apperrors.Internal("twilio.leg_unaddressable",
			"the provider accepted the call without returning a call sid; the leg cannot be managed")
	}
	holdURL := strings.TrimSpace(a.cfg.HoldTwiMLURL)
	if holdURL == "" {
		return apperrors.Internal("twilio.hold_not_configured",
			"hold requires a configured hold TwiML document (Config.HoldTwiMLURL)")
	}
	twiml := `<Response><Pause length="3600"/><Redirect method="POST">` + xmlText(holdURL) + `</Redirect></Response>`
	_, err := a.cl.post(ctx, a.callPath(l.callSID), url.Values{"Twiml": {twiml}})
	return err
}

// Resume implements telephony.VoiceProvider: the parked leg is redirected
// to the post-hold continuation document (Config.ResumeURL, falling back to
// the leg's original entry TwiML). Lifecycle-silent by contract.
func (a *Adapter) Resume(ctx context.Context, providerRef string) error {
	l, ok := a.leg(providerRef)
	if !ok {
		return errUnknownLeg
	}
	if l.callSID == "" {
		return apperrors.Internal("twilio.leg_unaddressable",
			"the provider accepted the call without returning a call sid; the leg cannot be managed")
	}
	target := strings.TrimSpace(a.cfg.ResumeURL)
	if target == "" {
		target = l.twimlURL
	}
	if target == "" {
		return apperrors.Invalid("twilio.resume_target_required",
			"resume requires a continuation document (Config.ResumeURL or the leg's entry TwiML)")
	}
	twiml := `<Response><Redirect method="POST">` + xmlText(target) + `</Redirect></Response>`
	_, err := a.cl.post(ctx, a.callPath(l.callSID), url.Values{"Twiml": {twiml}})
	return err
}

// deliver signs and delivers one translated provider event through the
// configured Ingest hook — the same signed-webhook entry the built-in
// Simulator uses, so the fail-closed gateway (O-15) treats real adapters
// and the simulator identically. A nil Ingest disables delivery (REST-only
// wiring); the events exist only through the hook otherwise.
func (a *Adapter) deliver(ctx context.Context, ev *comms.ProviderEvent) error {
	if a.cfg.Ingest == nil {
		return nil
	}
	body, err := ev.Encode()
	if err != nil {
		return apperrors.Internal("twilio.event_encode_failed",
			"could not encode the provider event").WithCause(err)
	}
	return a.cfg.Ingest(ctx, ProviderName, body, a.signer(body))
}

// leg returns the registered leg for a platform interaction id.
func (a *Adapter) leg(ref string) (*leg, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.legs[ref]
	return l, ok
}

// callsPath is the account-scoped call collection resource.
func (a *Adapter) callsPath() string {
	return fmt.Sprintf(apiCallsPath, a.cl.accountSID)
}

// callPath is one call instance resource.
func (a *Adapter) callPath(sid string) string {
	return fmt.Sprintf(apiCallPath, a.cl.accountSID, url.PathEscape(sid))
}

// callbackURL builds the self-scoping StatusCallback target: the externally
// routable callback base plus the interaction id and tenant as query
// passthrough. Twilio POSTs to the URL verbatim, query string included, so
// every callback arrives pre-scoped (events.go merges it with the body).
func callbackURL(base, interactionID, tenantID string) string {
	u := base + querySep(base) + url.QueryEscape("interaction") + "=" + url.QueryEscape(interactionID)
	return u + "&" + url.QueryEscape("tenant") + "=" + url.QueryEscape(tenantID)
}

// querySep picks the first query separator for a URL that may already carry
// a query string.
func querySep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

// xmlText escapes a value for safe embedding as TwiML element text
// (<Number>, <Redirect> targets).
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func xmlText(v string) string { return xmlEscaper.Replace(v) }
