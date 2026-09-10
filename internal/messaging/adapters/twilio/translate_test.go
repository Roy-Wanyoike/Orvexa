package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// callbackForm builds a realistic merged callback form: Twilio POST fields
// plus the routing keys the adapter put on the StatusCallback URL query.
func callbackForm(status string, interactionID, tenantID string) url.Values {
	return url.Values{
		"MessageSid":       {"SMfake000001"},
		"MessageStatus":    {status},
		"To":               {"%2B254711111111"},
		"From":             {"+15005550006"},
		QueryInteractionID: {interactionID},
		QueryTenantID:      {tenantID},
	}
}

// TestTranslateStatusMapping is the authoritative translation table:
// queued/sent → message.sent, delivered → message.delivered,
// undelivered/failed → message.failed, case-insensitively.
func TestTranslateStatusMapping(t *testing.T) {
	interaction := uuid.NewString()
	for _, tc := range []struct {
		twStatus string
		want     string
		wantDtl  string
	}{
		{"queued", evMessageSent, ""},
		{"sent", evMessageSent, ""},
		{"SENT", evMessageSent, ""},
		{"delivered", evMessageDelivered, ""},
		{"Delivered", evMessageDelivered, ""},
		{"undelivered", evMessageFailed, "twilio 30003: destination unreachable"},
		{"failed", evMessageFailed, "twilio 30005: carrier error"},
	} {
		t.Run(tc.twStatus, func(t *testing.T) {
			form := callbackForm(tc.twStatus, interaction, "tenant-1")
			if tc.wantDtl != "" {
				parts := strings.SplitN(tc.wantDtl, ": ", 2)
				form.Set("ErrorCode", parts[0][len("twilio "):])
				form.Set("ErrorMessage", parts[1])
			}
			ev, err := TranslateStatus(form)
			if err != nil {
				t.Fatalf("TranslateStatus: %v", err)
			}
			if ev.Event != tc.want {
				t.Errorf("status %q must map to %s, got %s", tc.twStatus, tc.want, ev.Event)
			}
			if ev.InteractionID != interaction {
				t.Errorf("event must carry interaction_id=%s, got %s", interaction, ev.InteractionID)
			}
			if ev.TenantID != "tenant-1" {
				t.Errorf("event must carry tenant_id=tenant-1, got %s", ev.TenantID)
			}
			if ev.Detail != tc.wantDtl {
				t.Errorf("detail must be %q, got %q", tc.wantDtl, ev.Detail)
			}
		})
	}
}

// TestTranslateStatusFailureReasonFallbacks proves message.failed always
// carries a reason, degrading honestly when Twilio omits fields.
func TestTranslateStatusFailureReasonFallbacks(t *testing.T) {
	interaction := uuid.NewString()
	for name, want := range map[string]string{
		"code only":    "twilio error 30003",
		"message only": "carrier refused the number",
		"neither":      "twilio undelivered",
	} {
		t.Run(name, func(t *testing.T) {
			form := callbackForm("undelivered", interaction, "tenant-1")
			switch name {
			case "code only":
				form.Set("ErrorCode", "30003")
			case "message only":
				form.Set("ErrorMessage", "carrier refused the number")
			}
			ev, err := TranslateStatus(form)
			if err != nil {
				t.Fatalf("TranslateStatus: %v", err)
			}
			if ev.Detail != want {
				t.Errorf("reason must be %q, got %q", want, ev.Detail)
			}
		})
	}
}

// TestTranslateStatusSmsStatusFallback proves the legacy SmsStatus field is
// consulted when MessageStatus is absent (Twilio sends both).
func TestTranslateStatusSmsStatusFallback(t *testing.T) {
	form := callbackForm("", uuid.NewString(), "tenant-1")
	form.Set("SmsStatus", "delivered")
	delete(form, "MessageStatus")
	ev, err := TranslateStatus(form)
	if err != nil {
		t.Fatalf("TranslateStatus: %v", err)
	}
	if ev.Event != evMessageDelivered {
		t.Errorf("SmsStatus fallback must map to %s, got %s", evMessageDelivered, ev.Event)
	}
}

// TestTranslateStatusMalformed proves routing-key validation is loud and
// speaks the processor's taxonomy.
func TestTranslateStatusMalformed(t *testing.T) {
	interaction := uuid.NewString()
	cases := map[string]url.Values{
		"missing status": {
			"MessageSid": {"SM1"}, QueryInteractionID: {interaction}, QueryTenantID: {"t"},
		},
		"missing sid": {
			"MessageStatus": {"sent"}, QueryInteractionID: {interaction}, QueryTenantID: {"t"},
		},
		"missing interaction": {
			"MessageSid": {"SM1"}, "MessageStatus": {"sent"}, QueryTenantID: {"t"},
		},
		"missing tenant": {
			"MessageSid": {"SM1"}, "MessageStatus": {"sent"}, QueryInteractionID: {interaction},
		},
		"non-uuid interaction": {
			"MessageSid": {"SM1"}, "MessageStatus": {"sent"},
			QueryInteractionID: {"not-a-uuid"}, QueryTenantID: {"t"},
		},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := TranslateStatus(form)
			if err == nil {
				t.Fatal("malformed callback must be rejected")
			}
			var ae *apperrors.Error
			if !errors.As(err, &ae) || ae.Code != "comms.malformed_callback" {
				t.Errorf("want comms.malformed_callback, got %v", err)
			}
		})
	}

	t.Run("unsupported status is loud", func(t *testing.T) {
		_, err := TranslateStatus(callbackForm("scheduled", interaction, "t"))
		var ae *apperrors.Error
		if !errors.As(err, &ae) || ae.Code != "comms.unknown_event" {
			t.Fatalf("want comms.unknown_event, got %v", err)
		}
		// The unsupported status is echoed (bounded) for debuggability.
		if !strings.Contains(err.Error(), "scheduled") {
			t.Errorf("rejection should name the status, got %v", err)
		}
	})
}

// TestMergeCallbackForm proves the merge contract: POST fields win, query
// fills gaps, inputs are never mutated.
func TestMergeCallbackForm(t *testing.T) {
	post := url.Values{"MessageSid": {"SM1"}, "MessageStatus": {"sent"}, "shared": {"post"}}
	query := url.Values{QueryInteractionID: {uuid.NewString()}, QueryTenantID: {"t"}, "shared": {"query"}}

	merged := MergeCallbackForm(post, query)

	if got := merged.Get("shared"); got != "post" {
		t.Errorf("POST fields must win collisions, got %q", got)
	}
	if merged.Get(QueryInteractionID) == "" || merged.Get(QueryTenantID) == "" {
		t.Errorf("query routing keys must survive the merge: %v", merged)
	}
	if post.Get(QueryInteractionID) != "" || query.Get("MessageStatus") != "" {
		t.Error("inputs must not be mutated")
	}
}

// TestHandleStatusCallbackNotWired proves the delivery path fails loudly
// when the adapter was built without the Ingest/Signer pair.
func TestHandleStatusCallbackNotWired(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.StatusCallbackBase = "" })
	err := a.HandleStatusCallback(context.Background(), callbackForm("sent", uuid.NewString(), "t"))
	if err == nil {
		t.Fatal("unwired adapter must reject callbacks")
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) || ae.Code != "twilio.ingest_not_wired" {
		t.Errorf("want twilio.ingest_not_wired, got %v", err)
	}
}

// TestStatusCallbackFullPathThroughHTTP is the receipt-path proof: a real
// send through the fake API, Twilio's async status callbacks POSTed over
// real HTTP to the adapter's callback receiver, translated into signed
// comms.ProviderEvent deliveries, and applied to the interaction lifecycle.
func TestStatusCallbackFullPathThroughHTTP(t *testing.T) {
	f := newFakeTwilio(t)
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, f, func(c *Config) {
		c.Ingest = rec.Ingest
		c.Signer = testSigner
	})
	f.setSink(a.HandleStatusCallback)

	msg := validMsg("tenant-1")
	// The platform creates the interaction and flips it active BEFORE the
	// provider is invoked (messaging.Service.Send); the recorder's guarded
	// lifecycle mirrors that strictness, so seed it the same way the
	// conformance kit does. Receipts for unseeded interactions are
	// hard not-found rejections by design.
	rec.Seed("tenant-1", msg.InteractionID, interactions.StatusActive)
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Twilio's async callbacks must arrive and apply within the window.
	if !rec.WaitApplied(2, 2*time.Second) {
		t.Fatalf("timed out waiting for receipt lifecycle, applied=%d", len(rec.Applied()))
	}

	evs := rec.AppliedFor("tenant-1", msg.InteractionID)
	if len(evs) != 2 || evs[0].Event != evMessageSent || evs[1].Event != evMessageDelivered {
		t.Fatalf("lifecycle must be exactly sent→delivered, got %+v", evs)
	}
	for _, ev := range evs {
		if ev.TenantID != "tenant-1" {
			t.Errorf("receipt must carry tenant_id=tenant-1, got %s", ev.TenantID)
		}
	}
	// The interaction stays ACTIVE: delivered never completes a conversation
	// (completed is reserved for the customer-side read receipt).
	if st, ok := rec.Status("tenant-1", msg.InteractionID); !ok || st != interactions.StatusActive {
		t.Errorf("interaction must remain active, got %s (ok=%v)", st, ok)
	}

	// Transport invariants on every delivery: provider label + signature
	// (the gateway routes by label and is fail-closed on signatures).
	for _, d := range rec.Deliveries() {
		if d.Provider != ProviderName {
			t.Errorf("provider label must be %q, got %q", ProviderName, d.Provider)
		}
		if d.Signature == "" {
			t.Errorf("delivery %q must carry a signature", d.Event.Event)
		}
	}

	// The fake observed the callback POSTs with routing keys merged from the
	// callback URL query (ParseForm semantics — what the webhook layer sees).
	cbs := f.firedCallbacks()
	if len(cbs) != 2 {
		t.Fatalf("two callback POSTs expected, got %d", len(cbs))
	}
	for _, cb := range cbs {
		if cb.Get(QueryInteractionID) != msg.InteractionID || cb.Get(QueryTenantID) != "tenant-1" {
			t.Errorf("callback must merge URL routing keys, got %v", cb)
		}
	}
}

// TestHandleStatusCallbackSurfacesDeliveryErrors proves the receipt path
// never swallows processing failures: an event the lifecycle rejects
// (unknown interaction) propagates so the webhook layer can answer 4xx.
func TestHandleStatusCallbackSurfacesDeliveryErrors(t *testing.T) {
	f := newFakeTwilio(t)
	rec := conformance.NewRecorder()
	a := newTestAdapter(t, f, func(c *Config) {
		c.Ingest = rec.Ingest
		c.Signer = testSigner
	})

	// No interaction seeded: the recorder's processor rejects as not-found.
	err := a.HandleStatusCallback(context.Background(), callbackForm("sent", uuid.NewString(), "tenant-1"))
	if err == nil {
		t.Fatal("delivery error must surface")
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) || ae.Code != "interaction.not_found" {
		t.Errorf("want interaction.not_found, got %v", err)
	}
	if got := len(rec.FailedDeliveries()); got != 1 {
		t.Errorf("the rejected delivery must be recorded, got %d", got)
	}
}

// TestHandleStatusCallbackProducesEncodedEvents sanity-checks the wire body:
// HandleStatusCallback must deliver a JSON-encoded comms.ProviderEvent.
func TestHandleStatusCallbackProducesEncodedEvents(t *testing.T) {
	ch := make(chan comms.ProviderEvent, 4)
	ingest := func(_ context.Context, provider string, body []byte, sig string) error {
		if provider != ProviderName || sig == "" {
			t.Errorf("delivery must be labelled %q and signed, got %q/%q", ProviderName, provider, sig)
		}
		var ev comms.ProviderEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Fatalf("body must be JSON-encoded ProviderEvent: %v (%s)", err, body)
		}
		ch <- ev
		return nil
	}
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.Ingest = ingest; c.Signer = testSigner })

	id := uuid.NewString()
	if err := a.HandleStatusCallback(context.Background(), callbackForm("failed", id, "tenant-1")); err != nil {
		t.Fatalf("HandleStatusCallback: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Event != evMessageFailed || ev.InteractionID != id {
			t.Errorf("encoded event mismatch: %+v", ev)
		}
		if ev.Detail == "" {
			t.Errorf("failed events must carry a reason, got %+v", ev)
		}
	default:
		t.Fatal("no event delivered")
	}
}
