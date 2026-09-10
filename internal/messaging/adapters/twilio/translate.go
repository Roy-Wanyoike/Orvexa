package twilio

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Platform event names the translation can emit. They are the closed
// vocabulary comms.Processor understands for the messaging plane.
const (
	evMessageSent      = "message.sent"
	evMessageDelivered = "message.delivered"
	evMessageFailed    = "message.failed"
)

// twilioStatusEvent maps Twilio MessageStatus values onto the platform's
// lifecycle events:
//
//	queued, sent      → message.sent   (carrier accepted / handed to network)
//	delivered         → message.delivered
//	undelivered, failed → message.failed (+ reason)
//
// Twilio's remaining states (accepted, scheduled, canceled, read, receiving,
// received, partially_delivered) are deliberately OUTSIDE the mapping: an
// unsupported status is a loud comms.unknown_event rejection, never a silent
// guess — the platform ethos is surfaced, not swallowed. In production the
// queued and sent callbacks both map to message.sent; the lifecycle
// processor's same-state no-op (comms.Processor) absorbs the repeat, and the
// conformance fake exercises the meaningful sent→delivered pair.
var twilioStatusEvent = map[string]string{
	"queued":      evMessageSent,
	"sent":        evMessageSent,
	"delivered":   evMessageDelivered,
	"undelivered": evMessageFailed,
	"failed":      evMessageFailed,
}

// TranslateStatus converts ONE Twilio messaging status callback into the
// platform's comms.ProviderEvent. This is the helper the webhook gateway
// layer consumes after fail-closed signature verification
// (internal/webhooks.VerifyTwilio): verify first, then translate.
//
// The form must carry Twilio's POST fields (MessageSid, MessageStatus, ...)
// PLUS the callback-URL query parameters that route the receipt
// (interaction_id, tenant_id — the keys the adapter put on StatusCallback).
// Twilio does not echo the query into the POST body, so callers merge the
// two views first: after r.ParseForm(), r.Form already IS the merged view;
// otherwise use MergeCallbackForm explicitly.
//
// The returned event carries:
//   - Event: the lifecycle mapping above;
//   - InteractionID / TenantID: the platform routing keys;
//   - Detail: a failure reason ("twilio <ErrorCode>: <ErrorMessage>") for
//     undelivered/failed only — it persists as the interaction end_reason;
//   - Timestamp: left empty; comms.ProviderEvent.Encode stamps arrival time
//     (Twilio status callbacks carry no timestamp field — documented honest
//     approximation).
func TranslateStatus(form url.Values) (*comms.ProviderEvent, error) {
	status := strings.ToLower(strings.TrimSpace(form.Get("MessageStatus")))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(form.Get("SmsStatus")))
	}
	sid := strings.TrimSpace(form.Get("MessageSid"))
	interactionID := strings.TrimSpace(form.Get(QueryInteractionID))
	tenantID := strings.TrimSpace(form.Get(QueryTenantID))

	if status == "" || sid == "" || interactionID == "" || tenantID == "" {
		return nil, apperrors.Invalid("comms.malformed_callback",
			"MessageStatus, MessageSid, interaction_id and tenant_id are required")
	}
	// comms.Processor requires interaction ids to be platform UUIDs; a
	// callback routed to a non-UUID id can never apply and must fail here,
	// not downstream.
	if _, err := uuid.Parse(interactionID); err != nil {
		return nil, apperrors.Invalid("comms.malformed_callback",
			"interaction_id must be a platform interaction id")
	}
	event, ok := twilioStatusEvent[status]
	if !ok {
		return nil, apperrors.Invalid("comms.unknown_event",
			fmt.Sprintf("unsupported Twilio MessageStatus %q", truncate(status, 64)))
	}
	return &comms.ProviderEvent{
		Event:         event,
		InteractionID: interactionID,
		TenantID:      tenantID,
		Detail:        failureReason(status, form),
	}, nil
}

// failureReason renders the failed-delivery reason carried in Detail (and
// persisted as the interaction end_reason). Presence of a reason is part of
// the message.failed contract; absence of Twilio's fields degrades to the
// bare status — never an empty reason on a failure.
func failureReason(status string, form url.Values) string {
	if status != "undelivered" && status != "failed" {
		return ""
	}
	code := strings.TrimSpace(form.Get("ErrorCode"))
	msg := strings.TrimSpace(form.Get("ErrorMessage"))
	switch {
	case code != "" && msg != "":
		return fmt.Sprintf("twilio %s: %s", code, truncate(msg, 200))
	case code != "":
		return "twilio error " + truncate(code, 32)
	case msg != "":
		return truncate(msg, 200)
	default:
		return "twilio " + status
	}
}

// MergeCallbackForm merges Twilio's POST fields with the callback-URL query
// parameters into a NEW url.Values (inputs are never mutated). POST fields
// win on collision — the query only fills gaps — because Twilio's payload is
// authoritative for its own keys while the platform routing keys exist only
// on the URL.
func MergeCallbackForm(postForm, query url.Values) url.Values {
	out := url.Values{}
	for k, vs := range postForm {
		out[k] = append([]string(nil), vs...)
	}
	for k, vs := range query {
		for _, v := range vs {
			if _, exists := out[k]; !exists {
				out.Set(k, v)
			}
		}
	}
	return out
}
