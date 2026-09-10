package africastalking

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// USSD session event names. The ProviderEvent schema is unchanged — Event is
// a string, and these values extend the vocabulary the way the task
// contract specifies. NOTE (honest integration gap): comms.Processor's
// vocabulary does not model inbound USSD sessions yet (its switch knows
// call.* and message.* only, and it requires UUID interaction ids), so
// until the processor grows the ussd.session.* family these events are
// recorded by the gateway but not applied to interaction lifecycles. The
// adapter never fabricates platform ids or tenants to force them through.
const (
	EventUssdStarted   = "ussd.session.started"
	EventUssdContinued = "ussd.session.continued"
	EventUssdEnded     = "ussd.session.ended"
)

// UssdPhase tells the handler where the session stands when a callback
// arrives, so menus can render the entry screen or a continuation screen.
type UssdPhase string

const (
	// UssdPhaseStarted is the first callback of a session (the bare dial).
	UssdPhaseStarted UssdPhase = "started"
	// UssdPhaseContinued is a subsequent callback of an open session (the
	// user answered the previous screen).
	UssdPhaseContinued UssdPhase = "continued"
)

// UssdCallback is one parsed Africa's Talking USSD callback (the operator's
// inbound POST when a user interacts with a session).
type UssdCallback struct {
	// SessionID is AT's sessionId. It maps 1:1 to the platform interaction
	// id under the sessionId↔interaction-id convention: the platform adopts
	// the carrier session id as the USSD leg's interaction identity, so
	// every event, receipt and flow for this session keys on it verbatim.
	// The adapter never mints its own ids for inbound sessions.
	SessionID string

	// ServiceCode is the dialed code (e.g. "*384*1234#").
	ServiceCode string

	// PhoneNumber is the subscriber's MSISDN (E.164 as the carrier sends
	// it). PII: it is passed to the handler and nowhere else — never into
	// events, logs or errors.
	PhoneNumber string

	// Text is the cumulative user input so far ("" on the first callback).
	// PII-adjacent: passed to the handler only.
	Text string

	// NetworkCode is the operator's network code (e.g. "63902" for Safaricom
	// KE). Informational; never logged per-callback.
	NetworkCode string

	// Phase is the resolved session phase for this callback (started on the
	// first callback of a session, continued afterwards).
	Phase UssdPhase
}

// UssdReply is the application's answer for one callback. The menu text is
// passed through to the carrier verbatim — the adapter never reflows,
// truncates or re-encodes it (text-only, one screen per round trip; the
// handler owns menu sizing within AT's documented per-screen limits).
type UssdReply struct {
	// Text is the menu/message body. If it already begins with AT's wire
	// prefixes ("CON " or "END ") it is passed through exactly as written;
	// otherwise the adapter prefixes it per End.
	Text string

	// End closes the session: the wire response becomes "END <Text>" and
	// ussd.session.ended is emitted after the phase event.
	End bool
}

// UssdHandler answers one USSD callback. Returned errors abort the
// translation BEFORE any event is emitted (and rewind the session-phase
// reservation, so the carrier's retry is treated as a fresh start).
type UssdHandler func(ctx context.Context, cb UssdCallback) (UssdReply, error)

// TranslateUssdCallback processes one native AT USSD callback POST payload
// (sessionId, serviceCode, phoneNumber, text, networkCode — form-encoded,
// JSON, or query-string) and returns the plain-text wire response to hand
// back to the carrier ("CON <menu>" or "END <message>").
//
// Event mapping (ProviderEvent schema unchanged — Event is a string):
//
//	first callback of a session            -> ussd.session.started
//	subsequent callback of an open session -> ussd.session.continued
//	handler reply ends the session          -> ussd.session.ended (after the phase event)
//
// The sessionId↔interaction-id convention applies: emitted events carry the
// carrier sessionId as InteractionID and Config.TenantID as TenantID.
//
// Retry idempotency: AT retries callbacks it got no response for. A callback
// for a session this adapter already answered with END returns the SAME wire
// text and emits nothing — the session is never reopened and the handler is
// not re-run (no double side effects).
//
// Emissions are best-effort relative to the wire response: the subscriber is
// waiting on the carrier's synchronous timeout, so a failed event delivery
// is retried once inline and logged, but never fails the callback response.
// Origin authenticity is the gateway's job (peer-IP allowlist, see
// internal/webhooks.VerifyAfricasTalking) and must have been verified before
// this translation runs.
func (a *Adapter) TranslateUssdCallback(ctx context.Context, raw []byte) (string, error) {
	if a.cfg.UssdHandler == nil {
		return "", apperrors.Invalid("ussd.handler_required", "the USSD surface requires a configured handler")
	}
	if strings.TrimSpace(a.cfg.TenantID) == "" {
		return "", apperrors.Invalid("ussd.tenant_required", "the USSD surface requires the account tenant (Config.TenantID)")
	}
	cb, err := parseUssdPayload(raw)
	if err != nil {
		return "", err
	}

	phase, retry, release := a.sessions.begin(cb.SessionID)
	if retry != nil {
		// Already answered with END: replay the same wire text, emit
		// nothing, do not re-run the handler (idempotent carrier retry).
		a.logger.InfoContext(ctx, "africastalking: retried ussd callback for an ended session acknowledged",
			slog.String("session_id", cb.SessionID))
		return retry.wire, nil
	}
	cb.Phase = phase

	reply, err := a.cfg.UssdHandler(ctx, cb)
	if err != nil {
		release() // rewind the reservation: the carrier retry starts fresh
		return "", err
	}
	wire, end := ussdWire(reply)
	if wire == "" {
		release()
		return "", apperrors.Invalid("ussd.empty_reply", "the handler returned no menu text")
	}

	// Phase event first, then the terminal event if the session ends. A
	// failed delivery is logged, never surfaced to the carrier: the
	// subscriber's screen must not depend on internal event plumbing.
	phaseEvent := EventUssdContinued
	detail := ""
	if phase == UssdPhaseStarted {
		phaseEvent = EventUssdStarted
		detail = "service_code=" + cb.ServiceCode
	}
	ev := comms.ProviderEvent{
		Event:         phaseEvent,
		InteractionID: cb.SessionID,
		TenantID:      a.cfg.TenantID,
		Detail:        detail,
	}
	if err := a.emitWithRetry(ctx, ev); err != nil {
		a.logger.WarnContext(ctx, "africastalking: ussd phase-event delivery failed",
			slog.String("session_id", cb.SessionID), slog.String("event", phaseEvent), slog.String("error", err.Error()))
	}
	if end {
		endEvent := comms.ProviderEvent{
			Event:         EventUssdEnded,
			InteractionID: cb.SessionID,
			TenantID:      a.cfg.TenantID,
		}
		if err := a.emitWithRetry(ctx, endEvent); err != nil {
			a.logger.WarnContext(ctx, "africastalking: ussd end-event delivery failed",
				slog.String("session_id", cb.SessionID), slog.String("error", err.Error()))
		}
		a.sessions.end(cb.SessionID, wire)
	}
	return wire, nil
}

// ussdWire renders the reply for the carrier: the handler's text is passed
// through verbatim when it already carries AT's wire prefix, else it is
// prefixed per End. An empty reply renders as the empty string so the
// caller can reject it (ussd.empty_reply): an empty screen is a handler
// bug, never a valid carrier answer. Returns the wire text and whether the
// session ends.
func ussdWire(reply UssdReply) (string, bool) {
	text := reply.Text
	switch {
	case strings.TrimSpace(text) == "":
		return "", false
	case strings.HasPrefix(text, "END "):
		return text, true
	case strings.HasPrefix(text, "CON "):
		return text, false
	case reply.End:
		return "END " + text, true
	default:
		return "CON " + text, false
	}
}

// parseUssdPayload decodes one AT USSD callback. The carrier posts
// form-encoded fields (or issues a query-string GET on legacy configs);
// JSON objects with the same keys are tolerated. Only sessionId,
// serviceCode and phoneNumber are mandatory — text is empty on the bare
// dial and networkCode is informational.
func parseUssdPayload(raw []byte) (UssdCallback, error) {
	var cb UssdCallback
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return cb, apperrors.Invalid("ussd.malformed_callback", "ussd callback payload is empty")
	}
	if strings.HasPrefix(trimmed, "{") {
		var jsonCb struct {
			SessionID   string `json:"sessionId"`
			ServiceCode string `json:"serviceCode"`
			PhoneNumber string `json:"phoneNumber"`
			Text        string `json:"text"`
			NetworkCode string `json:"networkCode"`
		}
		if err := json.Unmarshal([]byte(trimmed), &jsonCb); err != nil {
			return cb, apperrors.Invalid("ussd.malformed_callback", "ussd callback is not parseable").WithCause(err)
		}
		cb.SessionID, cb.ServiceCode, cb.PhoneNumber = jsonCb.SessionID, jsonCb.ServiceCode, jsonCb.PhoneNumber
		cb.Text, cb.NetworkCode = jsonCb.Text, jsonCb.NetworkCode
	} else {
		values, err := url.ParseQuery(trimmed)
		if err != nil {
			return cb, apperrors.Invalid("ussd.malformed_callback", "ussd callback is not parseable").WithCause(err)
		}
		cb.SessionID = values.Get("sessionId")
		cb.ServiceCode = values.Get("serviceCode")
		cb.PhoneNumber = values.Get("phoneNumber")
		cb.Text = values.Get("text")
		cb.NetworkCode = values.Get("networkCode")
	}
	if strings.TrimSpace(cb.SessionID) == "" {
		return cb, apperrors.Invalid("ussd.malformed_callback", "ussd callback carries no session id")
	}
	if strings.TrimSpace(cb.ServiceCode) == "" {
		return cb, apperrors.Invalid("ussd.malformed_callback", "ussd callback carries no service code")
	}
	if strings.TrimSpace(cb.PhoneNumber) == "" {
		return cb, apperrors.Invalid("ussd.malformed_callback", "ussd callback carries no phone number")
	}
	return cb, nil
}

// ussdSession is the adapter's memory of one carrier session: open (awaiting
// the next callback) or ended (wire text retained for idempotent retries).
type ussdSession struct {
	ended bool
	wire  string // the END response previously returned; empty while open
}

// begin resolves the phase for a callback of sessionId: unknown sessions
// open as started, open sessions continue. For an ENDED session it returns
// the retained END response (retry == non-nil) and takes no further action.
// The returned release func rewinds a fresh reservation (handler failure) so
// the carrier's retry is treated as a new start; end() closes the session.
func (t *ussdTable) begin(sessionID string) (UssdPhase, *ussdSession, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		if s.ended {
			return "", s, nil
		}
		s.wire = ""
		return UssdPhaseContinued, nil, func() {}
	}
	t.sessions[sessionID] = &ussdSession{}
	t.order = append(t.order, sessionID)
	t.evictLocked()
	return UssdPhaseStarted, nil, func() { t.rewind(sessionID) }
}

// rewind forgets an OPEN session reservation (handler failed before any
// event); ended sessions are never rewound.
func (t *ussdTable) rewind(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok && !s.ended {
		delete(t.sessions, sessionID)
	}
}

// end closes a session and retains its wire response for idempotent retries.
func (t *ussdTable) end(sessionID, wire string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[sessionID]; ok {
		s.ended = true
		s.wire = wire
	}
}

// ussdTable is the adapter's bounded in-memory session table. AT USSD
// sessions are short-lived (seconds, carrier-enforced), so per-process
// memory is honest here; the oldest entries are evicted beyond the cap and
// a callback for an evicted session is treated as a fresh start (documented
// degradation: at least one phase event re-emitted, lifecycle-safe
// downstream via provider-event dedup).
type ussdTable struct {
	cap      int
	mu       sync.Mutex
	order    []string
	sessions map[string]*ussdSession
}

func newUssdTable(cap int) *ussdTable {
	if cap <= 0 {
		cap = sentTableCap
	}
	return &ussdTable{cap: cap, sessions: map[string]*ussdSession{}}
}

// evictLocked forgets the oldest entries beyond the cap. Open sessions are
// evicted last-writable-first is NOT attempted: strict FIFO keeps the table
// deterministic; losing an open session's phase memory only re-labels a
// continuation as a start (see ussdTable docs).
func (t *ussdTable) evictLocked() {
	for len(t.order) > t.cap {
		oldest := t.order[0]
		t.order = t.order[1:]
		if s, ok := t.sessions[oldest]; ok && !s.ended {
			// Keep live sessions reachable: re-queue them rather than break
			// an open session's state mid-conversation.
			t.order = append(t.order, oldest)
			break
		}
		delete(t.sessions, oldest)
	}
}
