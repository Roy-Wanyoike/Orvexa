// USSD surface tests: TranslateUssdCallback's payload parsing (form, JSON,
// legacy query-string shapes and the malformed-callback taxonomy), the
// sessionId↔interaction-id event mapping, the full session lifecycle
// (started → continued → ended), the CON/END wire passthrough, retry
// idempotency for ended sessions, the handler-failure rewind, and the
// best-effort event-emission posture. Events are observed through the raw
// capture hook — the ussd.session.* vocabulary is intentionally not yet
// applied by comms.Processor (documented integration gap in the README), so
// the tests assert at the delivery layer, exactly where the webhook gateway
// records these events today.
package africastalking_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/africastalking"
)

const (
	testServiceCode = "*384*1234#"
	testNetworkCode = "63902"
)

// scriptedHandler records every parsed callback the adapter hands it and
// answers from a script keyed on the call index, so tests can count handler
// invocations (retry idempotency) and inspect the parsed UssdCallback.
type scriptedHandler struct {
	mu    sync.Mutex
	calls []africastalking.UssdCallback
	reply func(call int, cb africastalking.UssdCallback) (africastalking.UssdReply, error)
}

func (h *scriptedHandler) handle(_ context.Context, cb africastalking.UssdCallback) (africastalking.UssdReply, error) {
	h.mu.Lock()
	n := len(h.calls)
	h.calls = append(h.calls, cb)
	h.mu.Unlock()
	return h.reply(n, cb)
}

func (h *scriptedHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *scriptedHandler) call(i int) africastalking.UssdCallback {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[i]
}

// newUssdAdapter builds an adapter for the USSD surface wired to the raw
// capture hook (no processor — see the package comment) and the fixture
// credentials. BaseURL is a placeholder: TranslateUssdCallback never calls
// the carrier.
func newUssdAdapter(t *testing.T, cap *capture, handler africastalking.UssdHandler, mutate func(*africastalking.Config)) *africastalking.Adapter {
	t.Helper()
	cfg := africastalking.Config{
		Username:    registry.Secret("orvexa-sandbox"),
		APIKey:      registry.Secret(testAPIKey),
		SenderID:    registry.Secret(testSender),
		BaseURL:     "https://africastalking.test.invalid",
		Ingest:      cap.hook,
		Signer:      signBody("test-only-signing-secret"),
		TenantID:    testTenant,
		UssdHandler: handler,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ad, err := africastalking.New(cfg)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return ad
}

// ussdForm renders the carrier's canonical form-encoded callback payload.
func ussdForm(sessionID, text string) []byte {
	values := url.Values{}
	values.Set("sessionId", sessionID)
	values.Set("serviceCode", testServiceCode)
	values.Set("phoneNumber", testMSISDN)
	values.Set("text", text)
	values.Set("networkCode", testNetworkCode)
	return []byte(values.Encode())
}

// eventNames projects captured deliveries to their event names, in order.
func eventNames(caps *capture) []string {
	events, _, _, _ := caps.snapshot()
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = ev.Event
	}
	return names
}

// requireEvents asserts the exact captured event sequence.
func requireEvents(t *testing.T, caps *capture, want ...string) {
	t.Helper()
	got := eventNames(caps)
	if len(got) != len(want) {
		t.Fatalf("event sequence must be exactly %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event sequence must be exactly %v, got %v", want, got)
		}
	}
}

// TestUssdSessionLifecycleAndEvents walks one full session against the
// native form-encoded callback shape: the bare dial opens the session
// (started, Detail = the service code), an intermediate answer continues it,
// and an END reply closes it. Every delivery is labelled africastalking,
// signed, and carries the carrier sessionId as InteractionID and the
// account tenant as TenantID — the sessionId↔interaction-id convention.
// Subscriber PII (MSISDN, user input) never appears in any event body.
func TestUssdSessionLifecycleAndEvents(t *testing.T) {
	cap := &capture{}
	h := &scriptedHandler{reply: func(call int, cb africastalking.UssdCallback) (africastalking.UssdReply, error) {
		switch call {
		case 0:
			return africastalking.UssdReply{Text: "Welcome to Orvexa\n1. Balance"}, nil
		case 1:
			return africastalking.UssdReply{Text: "Balance is 42.50 KES"}, nil
		default:
			return africastalking.UssdReply{Text: "Goodbye", End: true}, nil
		}
	}}
	ad := newUssdAdapter(t, cap, h.handle, nil)
	ctx := context.Background()
	sid := "ATUid_atb5d_001"

	// 1) Bare dial: empty text, phase started.
	wire, err := ad.TranslateUssdCallback(ctx, ussdForm(sid, ""))
	if err != nil {
		t.Fatalf("TranslateUssdCallback(bare dial): %v", err)
	}
	if wire != "CON Welcome to Orvexa\n1. Balance" {
		t.Fatalf("bare dial must answer the prefixed entry menu, got %q", wire)
	}
	cb := h.call(0)
	if cb.Phase != africastalking.UssdPhaseStarted {
		t.Fatalf("first callback must present as started, got %q", cb.Phase)
	}
	if cb.SessionID != sid || cb.ServiceCode != testServiceCode ||
		cb.PhoneNumber != testMSISDN || cb.Text != "" || cb.NetworkCode != testNetworkCode {
		t.Fatalf("handler must receive the parsed callback verbatim, got %+v", cb)
	}
	evs, raws, sigs, labels := cap.snapshot()
	if len(evs) != 1 || evs[0].Event != africastalking.EventUssdStarted {
		t.Fatalf("bare dial must emit exactly ussd.session.started, got %v", eventNames(cap))
	}
	if evs[0].InteractionID != sid || evs[0].TenantID != testTenant {
		t.Fatalf("started event must carry sessionId/tenant, got interaction=%q tenant=%q", evs[0].InteractionID, evs[0].TenantID)
	}
	if evs[0].Detail != "service_code="+testServiceCode {
		t.Fatalf("started event Detail must be the service code, got %q", evs[0].Detail)
	}

	// 2) User answers "1": phase continued, no detail.
	wire, err = ad.TranslateUssdCallback(ctx, ussdForm(sid, "1"))
	if err != nil {
		t.Fatalf("TranslateUssdCallback(continue): %v", err)
	}
	if wire != "CON Balance is 42.50 KES" {
		t.Fatalf("continuation must answer the continuation screen, got %q", wire)
	}
	if cb := h.call(1); cb.Phase != africastalking.UssdPhaseContinued || cb.Text != "1" {
		t.Fatalf("second callback must present as continued with the cumulative text, got %+v", cb)
	}
	requireEvents(t, cap, africastalking.EventUssdStarted, africastalking.EventUssdContinued)
	if evs, _, _, _ = cap.snapshot(); evs[1].Detail != "" {
		t.Fatalf("continued event must carry no detail, got %q", evs[1].Detail)
	}

	// 3) Handler ends the session: END wire text, phase event then ended.
	wire, err = ad.TranslateUssdCallback(ctx, ussdForm(sid, "1*0"))
	if err != nil {
		t.Fatalf("TranslateUssdCallback(end): %v", err)
	}
	if wire != "END Goodbye" {
		t.Fatalf("ending reply must render END <text>, got %q", wire)
	}
	requireEvents(t, cap,
		africastalking.EventUssdStarted,
		africastalking.EventUssdContinued,
		africastalking.EventUssdContinued,
		africastalking.EventUssdEnded)

	// Transport invariants: every delivery labelled, signed, PII-free.
	evs, raws, sigs, labels = cap.snapshot()
	for i := range evs {
		if labels[i] != "africastalking" {
			t.Fatalf("delivery #%d must carry the provider label, got %q", i, labels[i])
		}
		if sigs[i] == "" {
			t.Fatalf("delivery #%d must carry a signature (fail-closed gateway)", i)
		}
		if strings.Contains(raws[i], testMSISDN) || strings.Contains(raws[i], "1*0") {
			t.Fatalf("event body #%d must never carry subscriber PII: %s", i, raws[i])
		}
	}
}

// TestUssdCallbackParsingTable walks the accepted payload shapes and the
// malformed-callback taxonomy: form-encoded (the carrier's canonical POST),
// JSON with the same keys, unknown JSON fields ignored, and every mandatory
// field enforced (sessionId, serviceCode, phoneNumber) before the handler is
// ever invoked.
func TestUssdCallbackParsingTable(t *testing.T) {
	fullJSON := `{"sessionId":"ATUid_j1","serviceCode":"*384*1234#","phoneNumber":"+254711223344","text":"hi","networkCode":"63902","extra":"ignored"}`
	cases := []struct {
		name    string
		raw     string
		wantErr string
		want    africastalking.UssdCallback
	}{
		{
			name: "form-encoded canonical shape",
			raw:  string(ussdForm("ATUid_f1", "1 2")),
			want: africastalking.UssdCallback{
				SessionID: "ATUid_f1", ServiceCode: testServiceCode,
				PhoneNumber: testMSISDN, Text: "1 2", NetworkCode: testNetworkCode,
				Phase: africastalking.UssdPhaseStarted,
			},
		},
		{
			name: "json with the same keys, unknown fields ignored",
			raw:  fullJSON,
			want: africastalking.UssdCallback{
				SessionID: "ATUid_j1", ServiceCode: testServiceCode,
				PhoneNumber: testMSISDN, Text: "hi", NetworkCode: testNetworkCode,
				Phase: africastalking.UssdPhaseStarted,
			},
		},
		{name: "empty payload", raw: "   ", wantErr: "ussd.malformed_callback"},
		{name: "no session id", raw: "serviceCode=*384*1234%23&phoneNumber=%2B254711223344", wantErr: "ussd.malformed_callback"},
		{name: "no service code", raw: "sessionId=ATUid_x&phoneNumber=%2B254711223344", wantErr: "ussd.malformed_callback"},
		{name: "no phone number", raw: "sessionId=ATUid_x&serviceCode=*384*1234%23", wantErr: "ussd.malformed_callback"},
		{name: "broken json", raw: `{"sessionId":"ATUid_x",`, wantErr: "ussd.malformed_callback"},
		{name: "broken form", raw: "%zz=not-a-query", wantErr: "ussd.malformed_callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capture{}
			h := &scriptedHandler{reply: func(int, africastalking.UssdCallback) (africastalking.UssdReply, error) {
				return africastalking.UssdReply{Text: "Menu"}, nil
			}}
			ad := newUssdAdapter(t, cap, h.handle, nil)

			wire, err := ad.TranslateUssdCallback(context.Background(), []byte(tc.raw))
			if tc.wantErr != "" {
				requireCode(t, err, tc.wantErr)
				if h.count() != 0 {
					t.Fatalf("a malformed callback must never reach the handler")
				}
				return
			}
			if err != nil {
				t.Fatalf("TranslateUssdCallback: %v", err)
			}
			if wire != "CON Menu" {
				t.Fatalf("wire must be the prefixed menu, got %q", wire)
			}
			if h.count() != 1 {
				t.Fatalf("handler must run exactly once, ran %d times", h.count())
			}
			if got := h.call(0); got != tc.want {
				t.Fatalf("parsed callback mismatch:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestUssdWirePassthroughTable pins the CON/END rendering rules: plain text
// is prefixed per End; text already carrying AT's wire prefixes is passed
// through verbatim and the prefix wins (a "CON …" reply never ends the
// session even with End set, an "END …" reply always ends it).
func TestUssdWirePassthroughTable(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		end      bool
		wantWire string
		wantEnd  bool
	}{
		{name: "plain text continues", text: "Menu", wantWire: "CON Menu"},
		{name: "plain text with End closes", text: "Done", end: true, wantWire: "END Done", wantEnd: true},
		{name: "CON prefix passes through verbatim", text: "CON Continue", wantWire: "CON Continue"},
		{name: "END prefix wins over End=false", text: "END Done", wantWire: "END Done", wantEnd: true},
		{name: "CON prefix wins over End=true", text: "CON Wait", end: true, wantWire: "CON Wait"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capture{}
			h := &scriptedHandler{reply: func(int, africastalking.UssdCallback) (africastalking.UssdReply, error) {
				return africastalking.UssdReply{Text: tc.text, End: tc.end}, nil
			}}
			ad := newUssdAdapter(t, cap, h.handle, nil)

			wire, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_wire", ""))
			if err != nil {
				t.Fatalf("TranslateUssdCallback: %v", err)
			}
			if wire != tc.wantWire {
				t.Fatalf("wire must be %q, got %q", tc.wantWire, wire)
			}
			got := eventNames(cap)
			if tc.wantEnd {
				if len(got) != 2 || got[1] != africastalking.EventUssdEnded {
					t.Fatalf("an END wire answer must close the session, got %v", got)
				}
				// The ended session's retry replays the same wire text.
				replay, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_wire", ""))
				if err != nil || replay != tc.wantWire {
					t.Fatalf("ended-session retry must replay %q with no error, got %q, %v", tc.wantWire, replay, err)
				}
				requireEvents(t, cap, africastalking.EventUssdStarted, africastalking.EventUssdEnded)
			} else {
				if got[len(got)-1] == africastalking.EventUssdEnded {
					t.Fatalf("a CON wire answer must leave the session open, got %v", got)
				}
			}
		})
	}
}

// TestUssdEndedSessionRetryIsIdempotent pins the carrier-retry contract: AT
// re-POSTs callbacks it got no response for. A callback for a session this
// adapter already answered with END replays the SAME wire text, emits
// nothing and does not re-run the handler — no double side effects.
func TestUssdEndedSessionRetryIsIdempotent(t *testing.T) {
	cap := &capture{}
	h := &scriptedHandler{reply: func(int, africastalking.UssdCallback) (africastalking.UssdReply, error) {
		return africastalking.UssdReply{Text: "Bye", End: true}, nil
	}}
	ad := newUssdAdapter(t, cap, h.handle, nil)
	ctx := context.Background()
	sid := "ATUid_retry"

	wire, err := ad.TranslateUssdCallback(ctx, ussdForm(sid, ""))
	if err != nil || wire != "END Bye" {
		t.Fatalf("first callback must end the session with END Bye, got %q, %v", wire, err)
	}
	requireEvents(t, cap, africastalking.EventUssdStarted, africastalking.EventUssdEnded)

	for i := 0; i < 3; i++ {
		replay, err := ad.TranslateUssdCallback(ctx, ussdForm(sid, ""))
		if err != nil {
			t.Fatalf("retry #%d: %v", i, err)
		}
		if replay != "END Bye" {
			t.Fatalf("retry #%d must replay the retained wire text, got %q", i, replay)
		}
	}
	if h.count() != 1 {
		t.Fatalf("the handler must never re-run for an ended session, ran %d times", h.count())
	}
	requireEvents(t, cap, africastalking.EventUssdStarted, africastalking.EventUssdEnded)
}

// TestUssdHandlerFailureRewinds pins the failure path: a handler error
// aborts the translation BEFORE any event is emitted and rewinds the
// session-phase reservation, so the carrier's retry is treated as a fresh
// start (started again, not continued).
func TestUssdHandlerFailureRewinds(t *testing.T) {
	cap := &capture{}
	handlerErr := errors.New("menu engine down")
	h := &scriptedHandler{reply: func(call int, cb africastalking.UssdCallback) (africastalking.UssdReply, error) {
		if call == 0 {
			return africastalking.UssdReply{}, handlerErr
		}
		return africastalking.UssdReply{Text: "Recovered"}, nil
	}}
	ad := newUssdAdapter(t, cap, h.handle, nil)
	ctx := context.Background()
	sid := "ATUid_fail"

	_, err := ad.TranslateUssdCallback(ctx, ussdForm(sid, ""))
	if !errors.Is(err, handlerErr) {
		t.Fatalf("the handler error must surface to the gateway untranslated, got %v", err)
	}
	if got := eventNames(cap); len(got) != 0 {
		t.Fatalf("a failed handler must emit nothing, got %v", got)
	}

	// The carrier retries the same session: it must present as started
	// again (reservation rewound), not continued.
	wire, err := ad.TranslateUssdCallback(ctx, ussdForm(sid, ""))
	if err != nil || wire != "CON Recovered" {
		t.Fatalf("retry after handler failure must be treated as a fresh start, got %q, %v", wire, err)
	}
	if cb := h.call(1); cb.Phase != africastalking.UssdPhaseStarted {
		t.Fatalf("the retried session must start over, got phase %q", cb.Phase)
	}
	requireEvents(t, cap, africastalking.EventUssdStarted)
}

// TestUssdSurfaceGuards pins the configuration guards: the USSD surface is
// active only with a handler AND the account tenant (inbound sessions carry
// no per-message tenant context, so events must be stampable).
func TestUssdSurfaceGuards(t *testing.T) {
	t.Run("no handler configured", func(t *testing.T) {
		cap := &capture{}
		ad := newUssdAdapter(t, cap, nil, nil)
		_, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_g1", ""))
		requireCode(t, err, "ussd.handler_required")
	})
	t.Run("no account tenant", func(t *testing.T) {
		cap := &capture{}
		h := &scriptedHandler{reply: func(int, africastalking.UssdCallback) (africastalking.UssdReply, error) {
			return africastalking.UssdReply{Text: "Menu"}, nil
		}}
		ad := newUssdAdapter(t, cap, h.handle, func(c *africastalking.Config) { c.TenantID = "" })
		_, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_g2", ""))
		requireCode(t, err, "ussd.tenant_required")
		if h.count() != 0 {
			t.Fatalf("a guarded callback must never reach the handler")
		}
	})
	t.Run("empty reply is rejected and rewinds", func(t *testing.T) {
		cap := &capture{}
		h := &scriptedHandler{reply: func(call int, _ africastalking.UssdCallback) (africastalking.UssdReply, error) {
			if call == 0 {
				return africastalking.UssdReply{}, nil
			}
			return africastalking.UssdReply{Text: "Recovered"}, nil
		}}
		ad := newUssdAdapter(t, cap, h.handle, nil)
		_, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_g3", ""))
		requireCode(t, err, "ussd.empty_reply")
		if got := eventNames(cap); len(got) != 0 {
			t.Fatalf("a rejected reply must emit nothing, got %v", got)
		}
		// The reservation is rewound: the carrier's retry starts fresh.
		wire, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_g3", ""))
		if err != nil || wire != "CON Recovered" {
			t.Fatalf("rewound session must be re-enterable, got %q, %v", wire, err)
		}
	})
}

// TestUssdEventPlumbingOutageIsBestEffort pins the emission posture: the
// subscriber is waiting on the carrier's synchronous timeout, so a failing
// event-delivery hook is retried inline and logged but NEVER fails the wire
// response.
func TestUssdEventPlumbingOutageIsBestEffort(t *testing.T) {
	cap := &capture{fail: true}
	h := &scriptedHandler{reply: func(int, africastalking.UssdCallback) (africastalking.UssdReply, error) {
		return africastalking.UssdReply{Text: "Menu"}, nil
	}}
	ad := newUssdAdapter(t, cap, h.handle, nil)

	wire, err := ad.TranslateUssdCallback(context.Background(), ussdForm("ATUid_outage", ""))
	if err != nil {
		t.Fatalf("event-plumbing failures must never fail the callback response, got %v", err)
	}
	if wire != "CON Menu" {
		t.Fatalf("the subscriber must still get the menu, got %q", wire)
	}
}

// TestUssdConcurrentSessions drives interleaved sessions through the
// adapter: every session starts, continues and ends exactly once, wires are
// per-session correct, and every delivery is labelled and signed. Run under
// -race this is the concurrency probe for the session table.
func TestUssdConcurrentSessions(t *testing.T) {
	cap := &capture{}
	h := &scriptedHandler{reply: func(_ int, cb africastalking.UssdCallback) (africastalking.UssdReply, error) {
		if cb.Text == "" {
			return africastalking.UssdReply{Text: "Menu"}, nil
		}
		return africastalking.UssdReply{Text: "Bye", End: true}, nil
	}}
	ad := newUssdAdapter(t, cap, h.handle, nil)

	const sessions = 8
	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(s int) {
			defer wg.Done()
			sid := fmt.Sprintf("ATUid_conc_%02d", s)
			wire, err := ad.TranslateUssdCallback(context.Background(), ussdForm(sid, ""))
			if err != nil || wire != "CON Menu" {
				t.Errorf("session %s first callback: wire %q, err %v", sid, wire, err)
				return
			}
			wire, err = ad.TranslateUssdCallback(context.Background(), ussdForm(sid, "1"))
			if err != nil || wire != "END Bye" {
				t.Errorf("session %s second callback: wire %q, err %v", sid, wire, err)
			}
		}(s)
	}
	wg.Wait()

	evs, _, sigs, labels := cap.snapshot()
	if len(evs) != sessions*3 {
		t.Fatalf("each session must emit started+continued+ended exactly once, got %d events for %d sessions", len(evs), sessions)
	}
	perSession := map[string]int{}
	for _, ev := range evs {
		perSession[ev.InteractionID]++
	}
	for s := 0; s < sessions; s++ {
		sid := fmt.Sprintf("ATUid_conc_%02d", s)
		if perSession[sid] != 3 {
			t.Fatalf("session %s must emit exactly 3 events, got %d", sid, perSession[sid])
		}
	}
	for i := range sigs {
		if sigs[i] == "" || labels[i] != "africastalking" {
			t.Fatalf("delivery #%d must be signed and labelled, got sig=%q label=%q", i, sigs[i], labels[i])
		}
	}
}

// compile-time pin: the capture hook must keep satisfying the platform's
// ingest port, and the adapter must keep implementing the messaging port.
var (
	_ comms.IngestFunc = (&capture{}).hook
)
