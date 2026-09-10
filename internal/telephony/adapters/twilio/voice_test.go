// Adapter tests: full lifecycle through the fake Twilio (REST + async status
// callbacks), the typed error taxonomy, bounded retry behavior, credential
// redaction, the callback translation table, and the conformance-kit wiring.
package twilio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Shared test credentials — long enough that registry.Redacted reveals a
// suffix, so the redaction test proves the bulk never leaks.
const (
	testAccountSID = "ACtestsid00000000000000aa1"
	testAuthToken  = "test0nly0token0000000000abcd"
	testTenant     = "tenant-under-test"
)

// newTestAdapter wires the full loop: fake Twilio (REST + async callbacks) →
// adapter → conformance Recorder. mutate adjusts the adapter configuration
// before construction (error-path and validation tests).
func newTestAdapter(t *testing.T, mutate func(*Config)) (*Adapter, *fakeTwilio, *conformance.Recorder) {
	t.Helper()
	fake := newFakeTwilio(testAccountSID, testAuthToken)
	t.Cleanup(fake.Close)
	rec := conformance.NewRecorder()

	cfg := Config{
		AccountSID:      testAccountSID,
		AuthToken:       testAuthToken,
		BaseURL:         fake.URL(),
		TwiMLBaseURL:    fake.URL() + "/twiml/entry",
		CallbackBaseURL: fake.URL() + "/callback",
		HoldTwiMLURL:    fake.URL() + "/twiml/hold",
		ResumeURL:       fake.URL() + "/twiml/resume",
		Ingest:          rec.Ingest,
		RetryBaseDelay:  2 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p := New(cfg)
	fake.SetCallbackHandler(http.HandlerFunc(p.ServeStatusCallback))
	return p, fake, rec
}

// placeCmd builds a valid dial command.
func placeCmd(id string) telephony.CallCommand {
	return telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	}
}

// seedAndPlace mirrors the core's contract (the interaction exists as
// pending BEFORE the provider is invoked — that is what the processor's
// strict lifecycle requires) and then dials.
func seedAndPlace(t *testing.T, p *Adapter, rec *conformance.Recorder, cmd telephony.CallCommand) {
	t.Helper()
	rec.Seed(testTenant, cmd.InteractionID, interactions.StatusPending)
	if err := p.PlaceCall(context.Background(), cmd); err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// eventNames projects applied events to their names.
func eventNames(evs []comms.ProviderEvent) []string {
	names := make([]string, len(evs))
	for i, ev := range evs {
		names[i] = ev.Event
	}
	return names
}

// equalSeq compares two sequences exactly.
func equalSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func assertSeq(t *testing.T, evs []comms.ProviderEvent, want ...string) {
	t.Helper()
	if got := eventNames(evs); !equalSeq(got, want) {
		t.Fatalf("event sequence must be exactly %v, got %v", want, got)
	}
}

func TestPlaceCallLifecycleE2E(t *testing.T) {
	p, fake, rec := newTestAdapter(t, nil)
	id := uuid.NewString()

	seedAndPlace(t, p, rec, placeCmd(id))
	waitFor(t, 2*time.Second, "ringing+connected deliveries", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})

	evs := rec.AppliedFor(testTenant, id)
	assertSeq(t, evs, "call.ringing", "call.connected")
	for i, ev := range evs {
		if ev.InteractionID != id || ev.TenantID != testTenant {
			t.Errorf("event #%d must be stamped interaction=%s tenant=%s, got interaction=%q tenant=%q",
				i, id, testTenant, ev.InteractionID, ev.TenantID)
		}
		if _, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err != nil {
			t.Errorf("event #%d timestamp %q must be RFC3339: %v", i, ev.Timestamp, err)
		}
	}
	for _, d := range rec.Deliveries() {
		if d.Provider != ProviderName {
			t.Errorf("delivery provider label must be %q, got %q", ProviderName, d.Provider)
		}
		if d.Signature == "" {
			t.Error("every delivery must carry a non-empty signature")
		}
	}

	// The REST place request must carry the full contract.
	places := fake.Places()
	if len(places) != 1 {
		t.Fatalf("exactly one place attempt expected, got %d", len(places))
	}
	form := places[0].Form
	if got := form.Get("To"); got != "+254711111111" {
		t.Errorf("To = %q", got)
	}
	if got := form.Get("From"); got != "+254700000001" {
		t.Errorf("From = %q", got)
	}
	if got := form.Get("Url"); got != fake.URL()+"/twiml/entry" {
		t.Errorf("Url (TwiML entry) = %q", got)
	}
	if got := form.Get("Method"); got != http.MethodPost {
		t.Errorf("Method = %q", got)
	}
	cb := form.Get("StatusCallback")
	wantCB := fake.URL() + "/callback?interaction=" + url.QueryEscape(id) + "&tenant=" + url.QueryEscape(testTenant)
	if cb != wantCB {
		t.Errorf("StatusCallback = %q, want %q", cb, wantCB)
	}
	if got := form.Get("StatusCallbackMethod"); got != http.MethodPost {
		t.Errorf("StatusCallbackMethod = %q", got)
	}
	if events := form["StatusCallbackEvent"]; !equalSeq(events, []string{"ringing", "answered", "completed"}) {
		t.Errorf("StatusCallbackEvent = %v", events)
	}

	// The fake delivered both callbacks asynchronously to the self-scoping URL.
	cbs := fake.Callbacks()
	if len(cbs) != 2 {
		t.Fatalf("two captured callback posts expected, got %d", len(cbs))
	}
	for i, want := range []string{"ringing", "in-progress"} {
		if got := cbs[i].Form.Get("CallStatus"); got != want {
			t.Errorf("callback #%d CallStatus = %q, want %q", i, got, want)
		}
		if got := cbs[i].Query.Get("interaction"); got != id {
			t.Errorf("callback #%d passthrough interaction = %q, want %q", i, got, id)
		}
		if got := cbs[i].Query.Get("tenant"); got != testTenant {
			t.Errorf("callback #%d passthrough tenant = %q, want %q", i, got, testTenant)
		}
	}

	if st, ok := rec.Status(testTenant, id); !ok || st != interactions.StatusActive {
		t.Errorf("interaction must be active after connect, got %v (ok=%v)", st, ok)
	}
}

func TestPlaceCallProviderOptions(t *testing.T) {
	id := uuid.NewString()

	p, fake, rec := newTestAdapter(t, nil)
	cmd := placeCmd(id)
	cmd.ProviderOptions["record"] = true
	cmd.ProviderOptions[optTwiMLURL] = fake.URL() + "/twiml/tenant-override"
	seedAndPlace(t, p, rec, cmd)
	waitFor(t, 2*time.Second, "lifecycle deliveries", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})
	form := fake.Places()[0].Form
	if got := form.Get("Record"); got != "true" {
		t.Errorf("Record = %q, want true", got)
	}
	if got := form.Get("Url"); got != fake.URL()+"/twiml/tenant-override" {
		t.Errorf("per-call twilio_url override ignored, Url = %q", got)
	}
}

func TestHangupEndsCallWithCause(t *testing.T) {
	p, fake, rec := newTestAdapter(t, nil)
	ctx := context.Background()
	id := uuid.NewString()

	seedAndPlace(t, p, rec, placeCmd(id))
	waitFor(t, 2*time.Second, "dial-phase events", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})
	if err := p.Hangup(ctx, id); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	waitFor(t, 2*time.Second, "call.ended delivery", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 3
	})

	evs := rec.AppliedFor(testTenant, id)
	assertSeq(t, evs, "call.ringing", "call.connected", "call.ended")
	if got := evs[len(evs)-1].Detail; got != "completed" {
		t.Errorf("call.ended Detail = %q, want the native cause %q", got, "completed")
	}

	updates := fake.Updates()
	if len(updates) != 1 {
		t.Fatalf("one status update expected, got %d", len(updates))
	}
	if got := updates[0].SID; got != fake.CallSID(-1) {
		t.Errorf("hangup addressed %q, want the minted sid %q", got, fake.CallSID(-1))
	}
	if got := updates[0].Form.Get("Status"); got != "completed" {
		t.Errorf("hangup Status = %q, want completed", got)
	}
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusWrapup {
		t.Errorf("interaction must be wrapup after ended, got %v", st)
	}
}

func TestTransferReRingsWithDestination(t *testing.T) {
	p, fake, rec := newTestAdapter(t, nil)
	ctx := context.Background()
	id := uuid.NewString()
	const dest = "+254722000000"

	seedAndPlace(t, p, rec, placeCmd(id))
	waitFor(t, 2*time.Second, "dial-phase events", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})
	if err := p.Transfer(ctx, id, dest); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	waitFor(t, 2*time.Second, "transfer ringing event", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 3
	})

	evs := rec.AppliedFor(testTenant, id)
	assertSeq(t, evs, "call.ringing", "call.connected", "call.ringing")
	if got := evs[len(evs)-1].Detail; !strings.Contains(got, dest) {
		t.Errorf("transfer ringing Detail = %q, want it to contain destination %q", got, dest)
	}
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusActive {
		t.Errorf("leg must stay active mid-transfer, got %v", st)
	}

	twiml := fake.Updates()[0].Form.Get("Twiml")
	if !strings.Contains(twiml, "<Dial><Number>"+dest+"</Number></Dial>") {
		t.Errorf("transfer Twiml = %q, want a <Dial><Number>%s</Number></Dial> redirect", twiml, dest)
	}

	if err := p.Hangup(ctx, id); err != nil {
		t.Fatalf("Hangup after transfer: %v", err)
	}
	waitFor(t, 2*time.Second, "post-transfer ended event", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 4
	})
	assertSeq(t, rec.AppliedFor(testTenant, id), "call.ringing", "call.connected", "call.ringing", "call.ended")
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusWrapup {
		t.Errorf("interaction must be wrapup after the transferred call ended, got %v", st)
	}
}

func TestHoldResumeLifecycleSilent(t *testing.T) {
	p, fake, rec := newTestAdapter(t, nil)
	ctx := context.Background()
	id := uuid.NewString()

	seedAndPlace(t, p, rec, placeCmd(id))
	waitFor(t, 2*time.Second, "dial-phase events", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})

	if err := p.Hold(ctx, id); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := p.Resume(ctx, id); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	rec.AwaitSilence(300 * time.Millisecond)
	if got := len(rec.AppliedFor(testTenant, id)); got != 2 {
		t.Fatalf("Hold/Resume must be lifecycle-silent, got %d events", got)
	}

	updates := fake.Updates()
	if len(updates) != 2 {
		t.Fatalf("hold+resume = two TwiML redirects, got %d updates", len(updates))
	}
	holdTwiml := updates[0].Form.Get("Twiml")
	if !strings.Contains(holdTwiml, `<Pause length="3600"/>`) {
		t.Errorf("hold Twiml = %q, want the <Pause length=\"3600\"/> park", holdTwiml)
	}
	if !strings.Contains(holdTwiml, `<Redirect method="POST">`+fake.URL()+"/twiml/hold"+`</Redirect>`) {
		t.Errorf("hold Twiml = %q, want a redirect back to the hold document", holdTwiml)
	}
	resumeTwiml := updates[1].Form.Get("Twiml")
	if !strings.Contains(resumeTwiml, `<Redirect method="POST">`+fake.URL()+"/twiml/resume"+`</Redirect>`) {
		t.Errorf("resume Twiml = %q, want a redirect to the continuation document", resumeTwiml)
	}

	if err := p.Hangup(ctx, id); err != nil {
		t.Fatalf("Hangup after hold/resume (leg must stay operable): %v", err)
	}
	waitFor(t, 2*time.Second, "ended event after hold/resume", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 3
	})
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusWrapup {
		t.Errorf("interaction must be wrapup, got %v", st)
	}
}

func TestResumeFallsBackToEntryTwiML(t *testing.T) {
	p, fake, rec := newTestAdapter(t, func(c *Config) { c.ResumeURL = "" })
	ctx := context.Background()
	id := uuid.NewString()

	seedAndPlace(t, p, rec, placeCmd(id))
	waitFor(t, 2*time.Second, "dial-phase events", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})
	if err := p.Hold(ctx, id); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := p.Resume(ctx, id); err != nil {
		t.Fatalf("Resume without ResumeURL: %v", err)
	}
	resumeTwiml := fake.Updates()[1].Form.Get("Twiml")
	if !strings.Contains(resumeTwiml, fake.URL()+"/twiml/entry") {
		t.Errorf("resume Twiml = %q, want fallback to the leg's entry TwiML", resumeTwiml)
	}
}

func TestUnknownRefOperationsFailTyped(t *testing.T) {
	p, fake, rec := newTestAdapter(t, nil)
	ctx := context.Background()
	ref := "conformance-unknown-" + uuid.NewString()

	type op struct {
		name string
		run  func() error
	}
	for _, o := range []op{
		{"Hangup", func() error { return p.Hangup(ctx, ref) }},
		{"Transfer", func() error { return p.Transfer(ctx, ref, "+254722000000") }},
		{"Hold", func() error { return p.Hold(ctx, ref) }},
		{"Resume", func() error { return p.Resume(ctx, ref) }},
	} {
		err1, err2 := o.run(), o.run()
		if err1 == nil || err2 == nil {
			t.Errorf("%s on unknown ref must fail deterministically, got %v / %v", o.name, err1, err2)
			continue
		}
		var ae1, ae2 *apperrors.Error
		if !asAppError(err1, &ae1) || !asAppError(err2, &ae2) {
			t.Errorf("%s on unknown ref must speak the application error model, got %T", o.name, err1)
			continue
		}
		if ae1.Kind != apperrors.KindNotFound {
			t.Errorf("%s on unknown ref must be not_found, got %q", o.name, ae1.Kind)
		}
		if ae1.Code != ae2.Code || ae1.Code != "twilio.unknown_leg" {
			t.Errorf("%s on unknown ref must be shape-stable code twilio.unknown_leg, got %q then %q", o.name, ae1.Code, ae2.Code)
		}
	}
	rec.AwaitSilence(200 * time.Millisecond)
	if got := len(rec.Deliveries()); got != 0 {
		t.Errorf("unknown-ref operations must emit nothing, got %d deliveries", got)
	}
	if len(fake.Places()) != 0 || len(fake.Updates()) != 0 {
		t.Error("unknown-ref operations must not touch the carrier REST API")
	}
}

func TestPlaceCallValidation(t *testing.T) {
	cases := []struct {
		name string
		cmd  telephony.CallCommand
		code string
	}{
		{"nil provider options", telephony.CallCommand{InteractionID: uuid.NewString(), From: "+254700000001", To: "+254711111111"}, "twilio.tenant_required"},
		{"missing tenant", telephony.CallCommand{InteractionID: uuid.NewString(), From: "+254700000001", To: "+254711111111", ProviderOptions: map[string]any{}}, "twilio.tenant_required"},
		{"non-string tenant", telephony.CallCommand{InteractionID: uuid.NewString(), From: "+254700000001", To: "+254711111111", ProviderOptions: map[string]any{"tenant_id": 42}}, "twilio.tenant_required"},
		{"missing interaction id", telephony.CallCommand{From: "+254700000001", To: "+254711111111", ProviderOptions: map[string]any{"tenant_id": testTenant}}, "twilio.interaction_required"},
		{"malformed from", mutatedCmd(func(c *telephony.CallCommand) { c.From = "not-a-phone" }), "twilio.invalid_phone_number"},
		{"too-short to", mutatedCmd(func(c *telephony.CallCommand) { c.To = "+12" }), "twilio.invalid_phone_number"},
		{"alphabetic to", mutatedCmd(func(c *telephony.CallCommand) { c.To = "+2547abc111" }), "twilio.invalid_phone_number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fake, rec := newTestAdapter(t, nil)
			err := p.PlaceCall(context.Background(), tc.cmd)
			var ae *apperrors.Error
			if !asAppError(err, &ae) {
				t.Fatalf("must reject with *apperrors.Error, got %v", err)
			}
			if ae.Kind != apperrors.KindInvalid {
				t.Errorf("kind must be invalid, got %q", ae.Kind)
			}
			if ae.Code != tc.code {
				t.Errorf("code = %q, want %q", ae.Code, tc.code)
			}
			if len(fake.Places()) != 0 {
				t.Error("a rejected command must not reach the carrier")
			}
			if got := len(rec.Deliveries()); got != 0 {
				t.Errorf("a rejected command must not emit deliveries, got %d", got)
			}
		})
	}

	t.Run("missing TwiML entry", func(t *testing.T) {
		p, fake, _ := newTestAdapter(t, func(c *Config) { c.TwiMLBaseURL = "" })
		err := p.PlaceCall(context.Background(), placeCmd(uuid.NewString()))
		assertInvalidCode(t, err, "twilio.twiml_url_required")
		if len(fake.Places()) != 0 {
			t.Error("a rejected command must not reach the carrier")
		}
	})
	t.Run("missing callback endpoint", func(t *testing.T) {
		p, fake, _ := newTestAdapter(t, func(c *Config) { c.CallbackBaseURL = "" })
		err := p.PlaceCall(context.Background(), placeCmd(uuid.NewString()))
		assertInvalidCode(t, err, "twilio.callback_url_required")
		if len(fake.Places()) != 0 {
			t.Error("a rejected command must not reach the carrier")
		}
	})
	t.Run("per-call TwiML override satisfies the entry requirement", func(t *testing.T) {
		p, fake, rec := newTestAdapter(t, func(c *Config) { c.TwiMLBaseURL = "" })
		cmd := placeCmd(uuid.NewString())
		cmd.ProviderOptions[optTwiMLURL] = fake.URL() + "/twiml/custom"
		seedAndPlace(t, p, rec, cmd)
		waitFor(t, 2*time.Second, "lifecycle events", func() bool {
			return len(rec.AppliedFor(testTenant, cmd.InteractionID)) == 2
		})
	})
}

// mutatedCmd is a tiny test helper for adjusting one command field.
func mutatedCmd(f func(*telephony.CallCommand)) telephony.CallCommand {
	cmd := placeCmd(uuid.NewString())
	f(&cmd)
	return cmd
}

func assertInvalidCode(t *testing.T, err error, want string) {
	t.Helper()
	var ae *apperrors.Error
	if !asAppError(err, &ae) {
		t.Fatalf("must reject with *apperrors.Error, got %v", err)
	}
	if ae.Kind != apperrors.KindInvalid || ae.Code != want {
		t.Fatalf("must be invalid/%s, got %s/%s", want, ae.Kind, ae.Code)
	}
}

func TestErrorTaxonomy(t *testing.T) {
	notFoundBody := `{"code":20404,"message":"The requested resource was not found","more_info":"https://www.twilio.com/docs/errors/20404","status":404}`
	rateLimitBody := `{"code":20429,"message":"Too many requests","more_info":"https://www.twilio.com/docs/errors/20429","status":429}`

	cases := []struct {
		name       string
		mutate     func(*Config)
		script     []fakeResponse
		wantKind   apperrors.Kind
		wantCode   string
		wantPlaces int
	}{
		{
			name:       "401 unauthorized is not retried",
			mutate:     func(c *Config) { c.AuthToken = "wrong-token-0000000000000000" },
			wantKind:   apperrors.KindUnauth,
			wantCode:   "twilio.auth_failed",
			wantPlaces: 1,
		},
		{
			name:       "404 not found is not retried",
			script:     []fakeResponse{{status: http.StatusNotFound, body: notFoundBody}},
			wantKind:   apperrors.KindNotFound,
			wantCode:   "twilio.resource_not_found",
			wantPlaces: 1,
		},
		{
			name: "429 rate limited exhausts the bounded retry budget",
			script: []fakeResponse{
				{status: http.StatusTooManyRequests, body: rateLimitBody, header: map[string]string{"Retry-After": "0"}},
				{status: http.StatusTooManyRequests, body: rateLimitBody, header: map[string]string{"Retry-After": "0"}},
				{status: http.StatusTooManyRequests, body: rateLimitBody, header: map[string]string{"Retry-After": "0"}},
				{status: http.StatusTooManyRequests, body: rateLimitBody, header: map[string]string{"Retry-After": "0"}},
			},
			wantKind:   apperrors.KindRateLimited,
			wantCode:   "twilio.rate_limited",
			wantPlaces: 4, // initial attempt + maxRetries
		},
		{
			name: "500 provider failure exhausts the bounded retry budget",
			script: []fakeResponse{
				{status: http.StatusInternalServerError, body: "upstream exploded"},
				{status: http.StatusInternalServerError, body: "upstream exploded"},
				{status: http.StatusInternalServerError, body: "upstream exploded"},
				{status: http.StatusInternalServerError, body: "upstream exploded"},
			},
			wantKind:   apperrors.KindInternal,
			wantCode:   "twilio.provider_error",
			wantPlaces: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, fake, rec := newTestAdapter(t, tc.mutate)
			if len(tc.script) > 0 {
				fake.ScriptPlace(tc.script...)
			}
			err := p.PlaceCall(context.Background(), placeCmd(uuid.NewString()))
			var ae *apperrors.Error
			if !asAppError(err, &ae) {
				t.Fatalf("must surface *apperrors.Error, got %v", err)
			}
			if ae.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", ae.Kind, tc.wantKind)
			}
			if ae.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ae.Code, tc.wantCode)
			}
			if got := len(fake.Places()); got != tc.wantPlaces {
				t.Errorf("place attempts = %d, want %d", got, tc.wantPlaces)
			}
			if got := len(rec.Deliveries()); got != 0 {
				t.Errorf("a failed dial must not emit deliveries, got %d", got)
			}
		})
	}
}

func TestRetryRecoversAfter429(t *testing.T) {
	p, fake, rec := newTestAdapter(t, nil)
	rateLimitBody := `{"code":20429,"message":"Too many requests","status":429}`
	fake.ScriptPlace(
		fakeResponse{status: http.StatusTooManyRequests, body: rateLimitBody, header: map[string]string{"Retry-After": "0"}},
		fakeResponse{status: http.StatusTooManyRequests, body: rateLimitBody, header: map[string]string{"Retry-After": "0"}},
	)

	ctx := context.Background()
	id := uuid.NewString()
	seedAndPlace(t, p, rec, placeCmd(id))
	if got := len(fake.Places()); got != 3 {
		t.Errorf("two 429s then success = 3 attempts, got %d", got)
	}
	waitFor(t, 2*time.Second, "lifecycle events after recovery", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})
	if err := p.Hangup(ctx, id); err != nil {
		t.Fatalf("Hangup on the recovered leg: %v", err)
	}
	waitFor(t, 2*time.Second, "ended event", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 3
	})
}

func TestConfigRedaction(t *testing.T) {
	cfg := Config{
		AccountSID:      "ACredacted0000000000000sid",
		AuthToken:       "supersecrettoken0000abcd",
		BaseURL:         "https://api.twilio.com",
		TwiMLBaseURL:    "https://example.com/twiml",
		CallbackBaseURL: "https://example.com/callback",
	}
	renderings := map[string]string{
		"String":   cfg.String(),
		"GoString": cfg.GoString(),
		"fmt %v":   fmt.Sprintf("%v", cfg),
		"fmt %s":   fmt.Sprintf("%s", cfg),
	}
	for name, s := range renderings {
		for _, secret := range []string{string(cfg.AccountSID), string(cfg.AuthToken), "supersecret"} {
			if strings.Contains(s, secret) {
				t.Errorf("%s rendering leaks credential material %q: %s", name, secret, s)
			}
		}
		if !strings.Contains(s, "****") {
			t.Errorf("%s rendering must mask credentials: %s", name, s)
		}
	}
	// Short secrets are fully masked (no suffix reveal below the threshold).
	s := Config{AuthToken: "short"}.String()
	if strings.Contains(s, "short") || !strings.Contains(s, "auth_token=****") {
		t.Errorf("short secrets must be fully masked, got: %s", s)
	}
}

func TestTranslateStatusCallbackTable(t *testing.T) {
	cases := []struct {
		name       string
		form       url.Values
		wantEvent  string
		wantDetail string
	}{
		{"queued", url.Values{"CallStatus": {"queued"}}, "call.ringing", ""},
		{"ringing", url.Values{"CallStatus": {"ringing"}}, "call.ringing", ""},
		{"in-progress", url.Values{"CallStatus": {"in-progress"}}, "call.connected", ""},
		{"completed", url.Values{"CallStatus": {"completed"}}, "call.ended", "completed"},
		{"no-answer", url.Values{"CallStatus": {"no-answer"}}, "call.ended", "no-answer"},
		{"busy", url.Values{"CallStatus": {"busy"}}, "call.ended", "busy"},
		{"failed", url.Values{"CallStatus": {"failed"}}, "call.ended", "failed"},
		{"canceled", url.Values{"CallStatus": {"canceled"}}, "call.ended", "canceled"},
		{"case-insensitive", url.Values{"CallStatus": {"IN-Progress"}}, "call.connected", ""},
		{"whitespace-tolerant", url.Values{"CallStatus": {" ringing "}}, "call.ringing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := TranslateStatusCallback(tc.form)
			if ev == nil {
				t.Fatal("expected a translated event, got nil")
			}
			if ev.Event != tc.wantEvent {
				t.Errorf("event = %q, want %q", ev.Event, tc.wantEvent)
			}
			if ev.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", ev.Detail, tc.wantDetail)
			}
		})
	}

	t.Run("no lifecycle meaning", func(t *testing.T) {
		for _, form := range []url.Values{
			{},
			{"CallStatus": {""}},
			{"CallStatus": {"in-route"}},
		} {
			if ev := TranslateStatusCallback(form); ev != nil {
				t.Errorf("TranslateStatusCallback(%v) = %+v, want nil", form, ev)
			}
		}
	})

	t.Run("self-scoping passthrough", func(t *testing.T) {
		ev := TranslateStatusCallback(url.Values{
			"CallStatus":  {"in-progress"},
			"interaction": {"inter-1"},
			"tenant":      {"tenant-1"},
			"Timestamp":   {"2026-02-14T10:00:00Z"},
		})
		if ev.InteractionID != "inter-1" || ev.TenantID != "tenant-1" {
			t.Errorf("passthrough not honored: interaction=%q tenant=%q", ev.InteractionID, ev.TenantID)
		}
		if ev.Timestamp != "2026-02-14T10:00:00Z" {
			t.Errorf("provider timestamp passthrough = %q", ev.Timestamp)
		}
	})
	t.Run("InteractionID fallback", func(t *testing.T) {
		ev := TranslateStatusCallback(url.Values{"CallStatus": {"ringing"}, "InteractionID": {"inter-2"}})
		if ev.InteractionID != "inter-2" {
			t.Errorf("InteractionID fallback = %q", ev.InteractionID)
		}
	})
}

func TestHandleStatusCallback(t *testing.T) {
	p, _, rec := newTestAdapter(t, nil)
	ctx := context.Background()
	id := uuid.NewString()

	seedAndPlace(t, p, rec, placeCmd(id))
	waitFor(t, 2*time.Second, "dial-phase events", func() bool {
		return len(rec.AppliedFor(testTenant, id)) == 2
	})

	t.Run("registry enrichment overrides a spoofed tenant", func(t *testing.T) {
		err := p.HandleStatusCallback(ctx,
			url.Values{"interaction": {id}, "tenant": {"spoiler"}},
			url.Values{"CallStatus": {"completed"}})
		if err != nil {
			t.Fatalf("HandleStatusCallback: %v", err)
		}
		evs := rec.AppliedFor(testTenant, id)
		if len(evs) != 3 || evs[2].Event != "call.ended" || evs[2].Detail != "completed" {
			t.Fatalf("expected call.ended delivered, got %+v", evs)
		}
		if evs[2].TenantID != testTenant {
			t.Errorf("registry tenant must win, got %q", evs[2].TenantID)
		}
	})

	t.Run("unscoped callback rejected", func(t *testing.T) {
		err := p.HandleStatusCallback(ctx, nil, url.Values{"CallStatus": {"ringing"}})
		assertInvalidCode(t, err, "twilio.callback_unscoped")
	})

	t.Run("unknown status is a no-op", func(t *testing.T) {
		before := len(rec.Deliveries())
		if err := p.HandleStatusCallback(ctx, url.Values{"interaction": {id}}, url.Values{"CallStatus": {"in-route"}}); err != nil {
			t.Fatalf("unknown status must not error, got %v", err)
		}
		if got := len(rec.Deliveries()); got != before {
			t.Errorf("unknown status must not deliver, got %d new", got-before)
		}
	})
}

func TestServeStatusCallbackHTTP(t *testing.T) {
	p, _, rec := newTestAdapter(t, nil)
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)

	srv := httptest.NewServer(http.HandlerFunc(p.ServeStatusCallback))
	t.Cleanup(srv.Close)

	t.Run("delivered callback answers 204", func(t *testing.T) {
		resp, err := http.PostForm(srv.URL+"/callback?interaction="+url.QueryEscape(id)+"&tenant="+url.QueryEscape(testTenant),
			url.Values{"CallStatus": {"ringing"}})
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
		waitFor(t, time.Second, "delivered event", func() bool {
			return len(rec.AppliedFor(testTenant, id)) == 1
		})
		if ev := rec.AppliedFor(testTenant, id)[0]; ev.Event != "call.ringing" {
			t.Errorf("event = %q, want call.ringing", ev.Event)
		}
	})

	t.Run("unscoped callback answers 400", func(t *testing.T) {
		resp, err := http.PostForm(srv.URL+"/callback", url.Values{"CallStatus": {"ringing"}})
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown status answers 204 without delivery", func(t *testing.T) {
		before := len(rec.Deliveries())
		resp, err := http.PostForm(srv.URL+"/callback?interaction="+id, url.Values{"CallStatus": {"in-route"}})
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
		if got := len(rec.Deliveries()); got != before {
			t.Errorf("unknown status must not deliver, got %d new", got-before)
		}
	})
}

// TestTwilioVoiceConformance embeds the provider conformance kit (the merge
// gate for every carrier adapter). Wiring: ONE recorder — the adapter's
// Ingest hook is rec.Ingest and the same rec is passed via Options — and the
// fake delivers native status callbacks asynchronously (goroutine, after the
// REST response), which is exactly why RequireAsyncEvents is set.
func TestTwilioVoiceConformance(t *testing.T) {
	rec := conformance.NewRecorder()
	fake := newFakeTwilio(testAccountSID, testAuthToken)
	t.Cleanup(fake.Close)

	p := New(Config{
		AccountSID:      testAccountSID,
		AuthToken:       testAuthToken,
		BaseURL:         fake.URL(),
		TwiMLBaseURL:    fake.URL() + "/twiml/entry",
		CallbackBaseURL: fake.URL() + "/callback",
		HoldTwiMLURL:    fake.URL() + "/twiml/hold",
		ResumeURL:       fake.URL() + "/twiml/resume",
		Ingest:          rec.Ingest,
		RetryBaseDelay:  2 * time.Millisecond,
	})
	fake.SetCallbackHandler(http.HandlerFunc(p.ServeStatusCallback))

	conformance.RunVoiceConformance(t, p, conformance.Options{
		Recorder:                 rec,
		ProviderName:             ProviderName,
		RequireAsyncEvents:       true,
		EventSettleTimeout:       500 * time.Millisecond,
		EnforceCommandValidation: true,
	})
}
