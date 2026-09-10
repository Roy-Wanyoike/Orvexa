package africastalking

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/customers"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Compile-time proof the adapter satisfies the port.
var _ telephony.VoiceProvider = (*Adapter)(nil)

// callbackBodyLimit bounds how much of a callback body is read before
// parsing; a hostile caller cannot balloon adapter memory.
const callbackBodyLimit = 8 << 10

// PlaceCall implements telephony.VoiceProvider: one make-call POST to
// AT /call carrying username, to (E.164 destination), from (callerId) and
// clientRequestId=<interaction id> (the routing key echoed on callbacks).
//
// The dial is asynchronous by carrier contract: a 2xx response with entry
// status "Queued" means AT accepted the dial — NOTHING has rung yet. The
// lifecycle (call.ringing → call.connected → call.ended) arrives later as
// status callbacks translated through HandleStatusCallback/TranslateStatus.
// This adapter therefore emits no events on the PlaceCall path itself.
//
// Validation gates (in order, all *apperrors.Error kind invalid, all
// rejected before any carrier contact): interaction id, tenant context,
// caller id, destination, E.164 shapes. A duplicate interaction id is a
// conflict — a second real POST would double-bill the customer, and the
// core never legitimately re-dials a live interaction.
func (a *Adapter) PlaceCall(ctx context.Context, cmd telephony.CallCommand) error {
	if strings.TrimSpace(cmd.InteractionID) == "" {
		return apperrors.Invalid("at.interaction_required", "interaction id is required")
	}
	tenantID, _ := cmd.ProviderOptions["tenant_id"].(string)
	if tenantID == "" {
		return apperrors.Invalid("comms.tenant_required", "tenant context is required")
	}
	if strings.TrimSpace(cmd.From) == "" {
		return apperrors.Invalid("at.caller_required", "from (callerId) is required")
	}
	if strings.TrimSpace(cmd.To) == "" {
		return apperrors.Invalid("at.destination_required", "to (destination) is required")
	}
	fromN, err := normalizePhone(cmd.From)
	if err != nil {
		return err
	}
	toN, err := normalizePhone(cmd.To)
	if err != nil {
		return err
	}

	a.mu.Lock()
	if _, dup := a.legs[cmd.InteractionID]; dup {
		a.mu.Unlock()
		return apperrors.Conflict("at.interaction_already_placed",
			"a call leg already exists for this interaction")
	}
	a.mu.Unlock()

	form := url.Values{}
	form.Set("username", string(a.username))
	form.Set("to", toN)
	form.Set("from", fromN)
	form.Set("clientRequestId", cmd.InteractionID)

	env, err := a.postCall(ctx, form)
	if err != nil {
		return err
	}
	if msg := cleanMsg(env.ErrorMessage); msg != "" {
		return apperrors.Conflict("at.call_rejected", "africastalking did not queue the call").
			WithDetails(map[string]any{"at_message": msg})
	}
	entry := selectEntry(env, toN)
	if entry == nil {
		return apperrors.Internal("at.envelope_invalid",
			"africastalking returned no call entry for the dialed number")
	}
	entryMsg := cleanMsg(entry.ErrorMessage)
	if entryMsg != "" || entry.Status != StatusQueued {
		return apperrors.Conflict("at.call_not_queued", "africastalking did not queue the call").
			WithDetails(map[string]any{
				"entry_status":     entry.Status,
				"at_entry_message": entryMsg,
			})
	}

	a.mu.Lock()
	leg := &atLeg{
		interactionID: cmd.InteractionID,
		tenantID:      tenantID,
		sessionID:     entry.SessionID,
	}
	a.legs[cmd.InteractionID] = leg
	if entry.SessionID != "" {
		a.sessions[entry.SessionID] = cmd.InteractionID
	}
	a.mu.Unlock()
	return nil
}

// selectEntry picks the envelope entry for the dialed number, falling back
// to the only entry when AT does not echo the number.
func selectEntry(env *callResponse, to string) *callEntry {
	if env == nil || len(env.Entries) == 0 {
		return nil
	}
	for i := range env.Entries {
		if normalizeLoose(env.Entries[i].PhoneNumber) == normalizeLoose(to) {
			return &env.Entries[i]
		}
	}
	if len(env.Entries) == 1 {
		return &env.Entries[0]
	}
	return nil
}

// normalizeLoose is the entry-matching view of a phone number: digits only,
// order preserved — enough to match AT's echo of the E.164 we dialed.
func normalizeLoose(v string) string {
	var b strings.Builder
	for _, r := range v {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Hangup implements telephony.VoiceProvider. AT exposes NO out-of-band REST
// hangup (research finding, README capability matrix), so the operator's
// request is dispatched in two honest layers:
//
//  1. platform-side: the leg is marked ended and a signed call.ended
//     (Detail "hangup") is delivered — the processor's audit trail records
//     the termination request at acceptance;
//  2. carrier-side: a no-further-actions response is queued for the
//     session's next callback exchange (AT ends the leg when its action
//     queue for the session runs out — best-effort by carrier contract;
//     confirmation arrives via the terminal status callback, which lands as
//     a same-state no-op through the processor).
//
// Hanging up an already-ended leg is an idempotent no-op (the requested
// end-state factually holds); an unknown reference is not_found. Never a
// faked success: the call.ended event records the platform request, and the
// carrier's own terminal callback remains the carrier truth.
func (a *Adapter) Hangup(ctx context.Context, providerRef string) error {
	leg, ok := a.lookupLeg(providerRef)
	if !ok {
		return apperrors.NotFound("at.unknown_leg", "unknown provider leg reference")
	}
	if leg.ended {
		return nil
	}
	a.markEnded(leg)
	a.queueAction(leg, controlAction{kind: actionHangup})
	return a.deliver(ctx, &comms.ProviderEvent{
		Event:         "call.ended",
		InteractionID: leg.interactionID,
		TenantID:      leg.tenantID,
		Detail:        "hangup",
	})
}

// Transfer implements telephony.VoiceProvider. AT has no out-of-band REST
// transfer; the transfer is dispatched platform-side (a new ringing phase
// on the same leg, destination recorded in Detail — the audit trail must
// show where a live customer was sent) and carrier-side via AT's documented
// <Dial> callback action, queued for the session's next callback exchange.
// Transfers apply to live legs only: a terminal leg cannot be re-rung, and
// AT exposes no API to act on a terminated session — that case is the typed
// ErrUnsupportedOperation, never a faked success.
func (a *Adapter) Transfer(ctx context.Context, providerRef, destination string) error {
	if strings.TrimSpace(destination) == "" {
		return apperrors.Invalid("at.transfer_destination_required", "transfer requires destination")
	}
	destN, err := normalizePhone(destination)
	if err != nil {
		return err
	}
	leg, ok := a.lookupLeg(providerRef)
	if !ok {
		return apperrors.NotFound("at.unknown_leg", "unknown provider leg reference")
	}
	if leg.ended {
		return ErrUnsupportedOperation
	}
	a.mu.Lock()
	leg.transferred = destN
	interactionID, tenantID := leg.interactionID, leg.tenantID
	a.mu.Unlock()
	a.queueAction(leg, controlAction{kind: actionTransfer, destination: destN})
	return a.deliver(ctx, &comms.ProviderEvent{
		Event:         "call.ringing",
		InteractionID: interactionID,
		TenantID:      tenantID,
		Detail:        "transfer:" + destN,
	})
}

// Hold implements telephony.VoiceProvider: a platform-side leg-state flip,
// lifecycle-silent (the ProviderEvent vocabulary has no hold semantics —
// inventing one would wrap up or re-ring a live call). AT has no native
// hold API; the carrier leg keeps its current flow (capability matrix,
// README) — media-level hold belongs to the deployment's AT call flow.
// Holding a terminal leg is the typed ErrUnsupportedOperation.
func (a *Adapter) Hold(ctx context.Context, providerRef string) error {
	leg, ok := a.lookupLeg(providerRef)
	if !ok {
		return apperrors.NotFound("at.unknown_leg", "unknown provider leg reference")
	}
	if leg.ended {
		return ErrUnsupportedOperation
	}
	a.mu.Lock()
	leg.held = true
	a.mu.Unlock()
	return nil
}

// Resume implements telephony.VoiceProvider (see Hold).
func (a *Adapter) Resume(ctx context.Context, providerRef string) error {
	leg, ok := a.lookupLeg(providerRef)
	if !ok {
		return apperrors.NotFound("at.unknown_leg", "unknown provider leg reference")
	}
	if leg.ended {
		return ErrUnsupportedOperation
	}
	a.mu.Lock()
	leg.held = false
	a.mu.Unlock()
	return nil
}

// lookupLeg returns the leg for a provider reference (interaction id).
func (a *Adapter) lookupLeg(ref string) (*atLeg, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	leg, ok := a.legs[ref]
	return leg, ok
}

// markEnded flips a leg to terminal under the registry lock.
func (a *Adapter) markEnded(leg *atLeg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	leg.ended = true
	leg.held = false
}

// Control actions: the carrier-side layer of in-call control, delivered in
// the adapter's next callback response for the session (AT's documented
// in-call control channel — the callBackUrl response body carries action
// tags AT executes).

// action kinds queued for carrier-side delivery.
const (
	actionHangup   = "hangup"
	actionTransfer = "transfer"
)

// controlAction is one queued carrier-side instruction.
type controlAction struct {
	kind        string
	destination string // transfer target (E.164)
}

// queueAction stores the leg's pending carrier-side action, replacing any
// earlier one (the latest operator intent wins — a transfer overtaking a
// queued hold is the operator's most recent instruction).
func (a *Adapter) queueAction(leg *atLeg, action controlAction) {
	a.mu.Lock()
	defer a.mu.Unlock()
	leg.action = &action
}

// drainAction returns and clears the leg's pending carrier-side action.
func (a *Adapter) drainAction(leg *atLeg) *controlAction {
	a.mu.Lock()
	defer a.mu.Unlock()
	act := leg.action
	leg.action = nil
	return act
}

// renderAction renders a queued action as AT callback-action XML. An empty
// action body is deliberate for hangup: with no further actions AT ends the
// leg when its session action queue runs out (documented best-effort — the
// carrier's terminal callback is the confirmation). Unknown kinds render as
// an empty no-action response rather than a guessed instruction.
func renderAction(act *controlAction) string {
	const open = `<?xml version="1.0" encoding="UTF-8"?><Response>`
	if act == nil {
		return open + `</Response>`
	}
	switch act.kind {
	case actionTransfer:
		return open + `<Dial phoneNumbers="` + act.destination + `"/></Response>`
	default:
		return open + `</Response>`
	}
}

// HandleStatusCallback translates one raw AT status callback body (JSON,
// issue #28 contract) into a signed ProviderEvent delivery. Routing keys:
// the clientRequestId echo (interaction id) first, then the adapter's
// session registry (sessionId → leg). A callback for a leg the adapter
// never placed — or whose session this process does not hold — is a typed
// not-found error, never a guessed route. Terminal statuses mark the leg
// ended (carrier truth). Signature/origin verification belongs to the
// fail-closed webhook gateway (AT callbacks are UNSIGNED; the gateway's
// peer-IP allowlist is the only origin control) — never here.
func (a *Adapter) HandleStatusCallback(ctx context.Context, body []byte) error {
	var cb StatusCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return apperrors.Invalid("at.callback_malformed", "status callback payload is not valid JSON").WithCause(err)
	}
	interactionID, tenantID, leg := a.routeCallback(&cb)
	ev, err := TranslateStatus(&cb, interactionID, tenantID)
	if err != nil {
		return err
	}
	if leg != nil && IsTerminal(cb.Status) {
		a.markEnded(leg)
	}
	return a.deliver(ctx, ev)
}

// routeCallback resolves the interaction/tenant routing keys for a
// callback: clientRequestId echo wins, then the session registry. A missing
// route yields empty strings — TranslateStatus turns that into the typed
// not-found error, so phantom callbacks can never materialize state.
func (a *Adapter) routeCallback(cb *StatusCallback) (interactionID, tenantID string, leg *atLeg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cb.ClientRequestID != "" {
		if l, ok := a.legs[cb.ClientRequestID]; ok {
			return l.interactionID, l.tenantID, l
		}
	}
	if cb.SessionID != "" {
		if id, ok := a.sessions[cb.SessionID]; ok {
			if l, ok := a.legs[id]; ok {
				return l.interactionID, l.tenantID, l
			}
		}
	}
	return "", "", nil
}

// deliver signs and delivers one ProviderEvent through the ingest port.
// A failed delivery is retried once — mirroring the built-in Simulator's
// carrier-faithful retry posture.
func (a *Adapter) deliver(ctx context.Context, ev *comms.ProviderEvent) error {
	body, err := ev.Encode()
	if err != nil {
		return apperrors.Internal("at.event_encode_failed", "lifecycle event could not be encoded").WithCause(err)
	}
	sig := a.signer(body)
	if err := a.ingest(ctx, ProviderName, body, sig); err != nil {
		return a.ingest(ctx, ProviderName, body, sig)
	}
	return nil
}

// StatusCallbackHandler returns the net/http handler for AT's callBackUrl
// surface. In production it mounts BEHIND the fail-closed webhook gateway
// (internal/webhooks.VerifyAfricasTalking — AT callbacks are unsigned, so
// the peer-IP allowlist is the only origin control); this handler itself
// performs NO origin verification. The response body carries the session's
// pending carrier-side action as AT callback-action XML (the documented
// in-call control channel), so a queued transfer/hangup reaches AT on the
// next callback exchange.
func (a *Adapter) StatusCallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, callbackBodyLimit))
		if err != nil {
			http.Error(w, `{"error":"callback unreadable"}`, http.StatusBadRequest)
			return
		}
		if err := a.HandleStatusCallback(r.Context(), body); err != nil {
			status := http.StatusInternalServerError
			var ae *apperrors.Error
			if errors.As(err, &ae) {
				status = ae.HTTPStatus()
			}
			http.Error(w, `{"error":"callback rejected"}`, status)
			return
		}
		// Carrier-side action delivery: the pending instruction (if any)
		// rides the callback response, exactly as AT's control protocol
		// expects. The action resolves against the session's leg when the
		// callback named one; otherwise it stays queued.
		var act *controlAction
		var cb StatusCallback
		if json.Unmarshal(body, &cb) == nil {
			_, _, leg := a.routeCallback(&cb)
			if leg != nil {
				act = a.drainAction(leg)
			}
		}
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(renderAction(act)))
	})
}

// normalizePhone canonicalizes a phone number to E.164 by reusing the
// platform's canonical identifier normalizer (internal/customers), so the
// adapter and the customer identity layer can never disagree about what a
// phone number is. The wrapped error keeps kind invalid under an
// adapter-local code.
func normalizePhone(v string) (string, error) {
	n, err := customers.NormalizeIdentifier(customers.IdentPhone, v)
	if err != nil {
		return "", apperrors.Wrap(err, apperrors.KindInvalid, "at.invalid_phone",
			"phone number must be 7-15 digits (E.164)")
	}
	return n, nil
}
