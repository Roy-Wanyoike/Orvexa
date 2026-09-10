package whatsappcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// This file implements the INBOUND half of the adapter (issue #27): Meta's
// statuses webhook translated into the platform's comms.ProviderEvent
// receipts. The webhook gateway (internal/webhooks VerifyWhatsAppCloud,
// x-hub-signature-256) verifies Meta's signature first and is fail-closed;
// the HTTP layer then hands the verified raw body to Provider.HandleWebhook.
//
// Translation contract:
//
//      status "sent"      -> message.sent      (interaction stays/becomes active)
//      status "delivered" -> message.delivered (active; never completes)
//      status "read"      -> message.read      (active -> completed)
//      status "failed"    -> message.failed    (active -> failed, reason in Detail)
//
// Statuses carry only the Graph message id (wamid); the send-side ledger
// (ledger.go) is what resolves it back to the platform interaction + tenant.
// Statuses for wamids this process never sent are reported as
// whatsapp.unknown_message_id and never attributed — a receipt must never
// move a conversation it cannot be proven to belong to.

// statusWebhook is the Cloud API statuses webhook envelope:
//
//	{"object":"whatsapp_business_account",
//	 "entry":[{"id":"<waba-id>","changes":[{"field":"messages",
//	   "value":{"messaging_product":"whatsapp",
//	            "metadata":{"display_phone_number":"...","phone_number_id":"..."},
//	            "statuses":[{"id":"wamid.<...>","status":"delivered",
//	                         "timestamp":"1664268184","recipient_id":"2547...",
//	                         "errors":[...]}]}}]}]}
//
// Meta batches: one payload may carry several entries, changes and statuses.
type statusWebhook struct {
	Object string         `json:"object"`
	Entry  []webhookEntry `json:"entry"`
}

type webhookEntry struct {
	ID      string          `json:"id"`
	Changes []webhookChange `json:"changes"`
}

type webhookChange struct {
	Field string       `json:"field"`
	Value webhookValue `json:"value"`
}

type webhookValue struct {
	MessagingProduct string           `json:"messaging_product"`
	Metadata         *webhookMetadata `json:"metadata"`
	Statuses         []graphStatus    `json:"statuses"`
}

// webhookMetadata is the value block's phone-number identity. It is parsed
// for operators and tests, but attribution is deliberately decided by the
// wamid ledger: a multi-number deployment runs one adapter per phone number
// id, and a status for a wamid this adapter never sent fails resolution
// (whatsapp.unknown_message_id) — the ledger IS the routing check.
type webhookMetadata struct {
	DisplayPhoneNumber string `json:"display_phone_number"`
	PhoneNumberID      string `json:"phone_number_id"`
}

// graphStatus is one statuses[] element: a receipt for one message.
type graphStatus struct {
	ID          string             `json:"id"`
	Status      string             `json:"status"`
	Timestamp   string             `json:"timestamp"` // unix seconds, as a string
	RecipientID string             `json:"recipient_id"`
	Errors      []graphStatusError `json:"errors"`
}

// graphStatusError is one failure reason on a failed status. None of these
// fields ever carry credential material (the access token travels only in
// OUR outbound requests; webhook payloads are authenticated by the app
// secret HMAC at the gateway, not by a token).
type graphStatusError struct {
	Code      int    `json:"code"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	FBTraceID string `json:"fbtrace_id"`
	ErrorData *struct {
		MessagingProduct string `json:"messaging_product"`
		Details          string `json:"details"`
	} `json:"error_data"`
}

// statusEventNames is the closed translation table from Graph status values
// to the platform event vocabulary (comms.Processor's contract). Anything
// outside the table is a loud error, not a guessed event.
var statusEventNames = map[string]string{
	"sent":      "message.sent",
	"delivered": "message.delivered",
	"read":      "message.read",
	"failed":    "message.failed",
}

// StatusResolver resolves a Graph wamid to the platform identity the
// original send registered. ok=false marks a wamid this adapter never sent
// (or cannot prove it sent). The signature keeps TranslateStatus pure: the
// resolver observes, never mutates.
type StatusResolver func(wamid string) (interactionID, tenantID string, ok bool)

// TranslateStatus translates a raw Graph statuses webhook body into
// ProviderEvents. It is a pure function: no ledger access, no I/O — the
// resolver callback supplies the send-side correlation so the returned
// events are complete (stamped with interaction_id + tenant_id) or the
// offending status is reported.
//
// Error semantics: per-status problems (unknown status value, missing id,
// unresolvable wamid) are joined — Meta batches statuses, so one bad entry
// must not block the honest ones in the same payload — and returned via
// errors.Join alongside the successfully translated events. A body that is
// not parsable JSON fails the whole call (whatsapp.webhook_malformed): there
// is nothing trustworthy to attribute in it.
//
// A body without statuses (e.g. an inbound-message change this adapter does
// not own) translates to (nil, nil) — nothing to attribute is not an error.
func TranslateStatus(body []byte, resolve StatusResolver) ([]*comms.ProviderEvent, error) {
	var wh statusWebhook
	if err := json.Unmarshal(body, &wh); err != nil {
		return nil, apperrors.Invalid("whatsapp.webhook_malformed",
			"webhook body is not a parsable Graph statuses payload").WithCause(err)
	}

	var (
		events []*comms.ProviderEvent
		errs   []error
	)
	for _, e := range wh.Entry {
		for _, c := range e.Changes {
			for i := range c.Value.Statuses {
				ev, err := translateStatus(&c.Value.Statuses[i], resolve)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				events = append(events, ev)
			}
		}
	}
	return events, errors.Join(errs...)
}

// translateStatus translates one statuses[] element.
func translateStatus(st *graphStatus, resolve StatusResolver) (*comms.ProviderEvent, error) {
	if st.ID == "" {
		return nil, apperrors.Invalid("whatsapp.webhook_status_missing_id",
			"statuses entry carries no message id; receipts cannot be correlated")
	}
	event, ok := statusEventNames[st.Status]
	if !ok {
		return nil, apperrors.Invalid("whatsapp.webhook_unknown_status",
			"unsupported Graph status "+strconv.Quote(st.Status))
	}
	var interactionID, tenantID string
	if resolve != nil {
		interactionID, tenantID, ok = resolve(st.ID)
	}
	if !ok || interactionID == "" || tenantID == "" {
		return nil, apperrors.NotFound("whatsapp.unknown_message_id",
			"status references a message id this adapter never sent; refusing to attribute it")
	}

	ev := &comms.ProviderEvent{
		Event:         event,
		InteractionID: interactionID,
		TenantID:      tenantID,
		Timestamp:     statusTimestamp(st.Timestamp),
	}
	if event == "message.failed" {
		// The failure reason rides in Detail (persisted as the interaction's
		// end_reason downstream). Operator-facing, client-safe by construction.
		ev.Detail = failureDetail(st.Errors)
	}
	return ev, nil
}

// statusTimestamp converts Graph's unix-seconds string to RFC3339 UTC (the
// ProviderEvent wire format). An absent or unparsable timestamp yields ""
// and the delivery path stamps ingest time instead — a receipt with a
// provider-side time is preferred, but never worth failing a delivery over.
func statusTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs < 0 {
		return ""
	}
	return time.Unix(secs, 0).UTC().Format(time.RFC3339)
}

// failureDetail renders the operator-facing reason from a failed status's
// errors array: the Graph code plus Meta's title/message and error_data
// details when present. Deterministic and credential-free by construction.
func failureDetail(errs []graphStatusError) string {
	if len(errs) == 0 {
		return "provider reported failure without error details"
	}
	e := errs[0] // the first error is Meta's primary reason; the rest are context
	d := fmt.Sprintf("graph code %d", e.Code)
	switch {
	case e.Title != "" && e.Message != "" && e.Title != e.Message:
		d += ": " + e.Title + ": " + e.Message
	case e.Title != "":
		d += ": " + e.Title
	case e.Message != "":
		d += ": " + e.Message
	}
	if e.ErrorData != nil && e.ErrorData.Details != "" {
		d += " (" + e.ErrorData.Details + ")"
	}
	return d
}

// HandleWebhook is the inbound status path: it translates the verified raw
// webhook body into ProviderEvents and delivers each one through the signed
// ingest hook (Signer + Ingest — the same fail-closed delivery the Simulator
// uses). It is synchronous: when it returns, every translatable status in
// the payload has been delivered or reported.
//
// Error semantics (joined, never silently swallowed):
//
//   - translation problems (unknown status, unresolvable wamid, malformed
//     body) — reported per TranslateStatus;
//   - ingest/processor rejections (unknown interaction, illegal transition,
//     gateway outage) — reported as-is: the processor is the lifecycle's
//     authority and its verdicts are data, not noise;
//   - valid statuses in a batch are delivered even when siblings fail.
func (p *Provider) HandleWebhook(ctx context.Context, body []byte) error {
	events, terr := TranslateStatus(body, p.ledger.resolver())

	var errs []error
	if terr != nil {
		errs = append(errs, terr)
	}
	for _, ev := range events {
		wire, err := ev.Encode() // stamps the provider-side timestamp when absent
		if err != nil {
			errs = append(errs, apperrors.Internal("whatsapp.receipt_encode_failed",
				"translated receipt could not be encoded").WithCause(err))
			continue
		}
		if err := p.cfg.Ingest(ctx, ProviderName, wire, p.cfg.Signer(wire)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// resolver adapts the send-side ledger to the StatusResolver port.
func (l *msgLedger) resolver() StatusResolver {
	return func(wamid string) (interactionID, tenantID string, ok bool) {
		ref, ok := l.lookup(wamid)
		if !ok {
			return "", "", false
		}
		return ref.interactionID, ref.tenantID, true
	}
}
