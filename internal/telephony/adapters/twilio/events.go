// Status-callback translation: Twilio's native form-encoded voice status
// callbacks mapped onto the platform's comms.ProviderEvent vocabulary and
// delivered through the same signed-webhook entry the built-in Simulator
// uses.
//
//	queued, ringing            -> call.ringing
//	in-progress                -> call.connected
//	completed                  -> call.ended (Detail: native cause "completed")
//	no-answer, busy,
//	failed, canceled           -> call.ended (Detail: native reason)
//
// Every terminal Twilio status maps to call.ended — the platform's
// lifecycle has a single customer-left transition (wrapup) and the native
// reason rides in Detail, which the processor persists as end_reason.
package twilio

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Native callback fields this adapter consumes (Twilio's form encoding),
// plus the self-scoping passthrough parameters PlaceCall appends to the
// StatusCallback URL.
const (
	fieldCallStatus   = "CallStatus"
	fieldInteraction  = "interaction" // passthrough: platform interaction id
	fieldTenant       = "tenant"      // passthrough: platform tenant id
	fieldCallbackTime = "Timestamp"   // optional provider-side occurrence time
)

// TranslateStatusCallback maps one native voice status callback (form view
// with query passthrough already merged by the caller) onto a
// comms.ProviderEvent, reading the interaction id and tenant from the
// self-scoping passthrough fields.
//
// A nil return means the callback carries no lifecycle meaning this adapter
// understands (missing or unknown CallStatus) — a no-op, not an error:
// carriers add statuses over time and the gateway must not fail on new
// vocabulary. Terminal statuses always carry the native cause in Detail
// (the processor persists it as the interaction's end_reason).
func TranslateStatusCallback(form url.Values) *comms.ProviderEvent {
	status := strings.ToLower(strings.TrimSpace(form.Get(fieldCallStatus)))
	if status == "" {
		return nil
	}
	var event, detail string
	switch status {
	case "queued", "ringing":
		event = "call.ringing"
	case "in-progress":
		event = "call.connected"
	case "completed":
		event, detail = "call.ended", "completed"
	case "no-answer", "busy", "failed", "canceled":
		event, detail = "call.ended", status
	default:
		return nil
	}
	ev := &comms.ProviderEvent{
		Event:         event,
		Detail:        detail,
		InteractionID: firstNonEmpty(form.Get(fieldInteraction), form.Get("InteractionID")),
		TenantID:      strings.TrimSpace(form.Get(fieldTenant)),
	}
	if ts := strings.TrimSpace(form.Get(fieldCallbackTime)); ts != "" {
		ev.Timestamp = ts
	}
	return ev
}

// HandleStatusCallback processes one native voice status callback: translate
// it, enrich it from the leg registry, and deliver it through the Ingest
// hook. This is the production entry the fail-closed webhook gateway calls
// AFTER verifying X-Twilio-Signature (O-15) — the adapter performs no
// signature verification at this layer.
//
// Tenant enrichment: the leg registry is authoritative for legs this process
// placed, but PlaceCall's self-scoping passthrough keeps delivery correct
// even for callbacks that race leg registration or arrive after a process
// restart (the query rides on the carrier's POST verbatim).
//
// Returns a typed invalid error for unscoped callbacks (no interaction id) —
// the gateway answers 400 and the rejection is visible, never silently
// swallowed. Unknown statuses are a nil no-op.
func (a *Adapter) HandleStatusCallback(ctx context.Context, query, form url.Values) error {
	ev := TranslateStatusCallback(mergeForm(query, form))
	if ev == nil {
		return nil
	}
	if ev.InteractionID == "" {
		return apperrors.Invalid("twilio.callback_unscoped",
			"voice status callback carries no interaction id (missing self-scoping passthrough)")
	}
	if l, ok := a.leg(ev.InteractionID); ok {
		ev.TenantID = l.tenantID
	}
	return a.deliver(ctx, ev)
}

// ServeStatusCallback is the http.Handler the webhook gateway (or a test
// double) forwards VERIFIED Twilio voice callbacks to. It merges the URL
// query (self-scoping interaction/tenant passthrough) with the POST body
// (native Twilio fields) and delegates to HandleStatusCallback.
//
// Responses: 204 No Content on delivered and no-op callbacks, 400 for
// malformed forms and unscoped callbacks, 502 when the ingest hook rejects
// the delivery.
func (a *Adapter) ServeStatusCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed callback form", http.StatusBadRequest)
		return
	}
	err := a.HandleStatusCallback(r.Context(), r.URL.Query(), r.PostForm)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var ae *apperrors.Error
	if asAppError(err, &ae) && ae.Kind == apperrors.KindInvalid {
		http.Error(w, "unscoped voice status callback", http.StatusBadRequest)
		return
	}
	http.Error(w, "voice status callback delivery failed", http.StatusBadGateway)
}

// mergeForm folds query parameters and the POST body into one view, query
// first: url.Values.Get returns the first value, so the self-scoping
// passthrough wins over any same-named body field.
func mergeForm(query, form url.Values) url.Values {
	merged := url.Values{}
	for k, vs := range query {
		merged[k] = append(merged[k], vs...)
	}
	for k, vs := range form {
		merged[k] = append(merged[k], vs...)
	}
	return merged
}

// firstNonEmpty returns the first value that is non-blank after trimming.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
