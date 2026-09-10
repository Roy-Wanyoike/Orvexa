// SMS adapter tests: the conformance kit (the authoritative behavioral
// contract), the carrier wire contract against the fake AT server, the
// delivery-report translation paths, the bounded retry budget, send
// idempotency, strict E.164 addressing, and zero credential/PII leakage.
package africastalking_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/africastalking"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// TestATSMSConformance embeds the provider conformance kit. The fake carrier
// answers with AT's synchronous status 102 (Delivered) — a real carrier
// response on networks that confirm inline — so the kit's synchronous
// sent→delivered contract holds. The adapter also re-validates every message
// through the core gate, so the full validation taxonomy is opted into.
func TestATSMSConformance(t *testing.T) {
	f := newFakeAT(t)
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)

	conformance.RunMessagingConformance(t, ad, conformance.Options{
		Recorder:              rec,
		ProviderName:          "africastalking",
		EnforceSendValidation: true,
		TenantID:              testTenant,
	})
}

// validMsg returns a valid tenant-bound SMS for the fake-carrier tests.
func validMsg() messaging.Message {
	return messaging.Message{
		InteractionID: uuid.NewString(),
		TenantID:      testTenant,
		Channel:       messaging.ChannelSMS,
		To:            testMSISDN,
		Body:          "orvexa fake-carrier ping",
	}
}

// TestSendLifecycleViaDeliveryReport pins the async delivery path: sync
// status 101 (Sent) yields message.sent only; the out-of-band delivery
// report (AT's documented JSON POST) completes the lifecycle.
func TestSendLifecycleViaDeliveryReport(t *testing.T) {
	f := newFakeAT(t)
	f.recipStatus, f.recipStatusText = 101, "Sent"
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)

	msg := validMsg()
	seedInteraction(rec, msg)
	if err := ad.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	evs := rec.AppliedFor(testTenant, msg.InteractionID)
	if len(evs) != 1 || evs[0].Event != "message.sent" {
		t.Fatalf("sync 101 acceptance must emit exactly message.sent, got %v", evs)
	}

	report := fmt.Sprintf(`{"id":%q,"status":"Success","phoneNumber":%q,"networkCode":"63902"}`,
		f.messageID(0), testMSISDN)
	if err := ad.TranslateDelivery(context.Background(), "application/json", []byte(report)); err != nil {
		t.Fatalf("TranslateDelivery(Success): %v", err)
	}

	evs = rec.AppliedFor(testTenant, msg.InteractionID)
	if len(evs) != 2 || evs[0].Event != "message.sent" || evs[1].Event != "message.delivered" {
		t.Fatalf("lifecycle must be sent→delivered, got %v", evs)
	}
	if st, ok := rec.Status(testTenant, msg.InteractionID); !ok || string(st) != "active" {
		t.Fatalf("interaction must stay active after delivery, got %v (ok=%v)", st, ok)
	}
}

// TestCarrierWireContract pins the exact request the adapter must send:
// version-1 messaging path, JSON body with username/to/message/from, the
// apiKey header carrying the credential, sender-ID mapping, and strict E.164
// destination canonicalization.
func TestCarrierWireContract(t *testing.T) {
	f := newFakeAT(t)
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)

	msg := validMsg()
	msg.To = "254711223344" // international form without '+' — canonicalized
	msg.From = ""           // registry sender ID applies
	if err := ad.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sent := f.recorded()
	if len(sent) != 1 {
		t.Fatalf("exactly one carrier POST expected, got %d", len(sent))
	}
	s := sent[0]
	if s.Path != "/messaging" || s.Method != http.MethodPost {
		t.Fatalf("path must be POST /messaging, got %s %s", s.Method, s.Path)
	}
	if s.APIKey != testAPIKey {
		t.Fatalf("apiKey header must carry the credential, got %q", s.APIKey)
	}
	if s.Username != "orvexa-sandbox" {
		t.Fatalf("username must be the account username, got %q", s.Username)
	}
	if s.To != testMSISDN {
		t.Fatalf("destination must be canonicalized to E.164, got %q", s.To)
	}
	if s.From != testSender {
		t.Fatalf("sender ID must default to the registry sender, got %q", s.From)
	}
	if s.Message != msg.Body {
		t.Fatalf("message body must pass through, got %q", s.Message)
	}
	if ct := s.Headers.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("carrier request must be JSON, got Content-Type %q", ct)
	}

	// Per-message From overrides the registry sender ID.
	msg2 := validMsg()
	msg2.From = "ORVEXA-OVERRIDE"
	if err := ad.Send(context.Background(), msg2); err != nil {
		t.Fatalf("Send(override): %v", err)
	}
	if got := f.recorded()[1].From; got != "ORVEXA-OVERRIDE" {
		t.Fatalf("per-message From must override the sender ID, got %q", got)
	}
}

// TestDeliveryReportStatusMapping walks the report-status table: terminal
// deliverable, terminal failures (with reason passthrough), and intermediate
// statuses that must be acknowledged without inventing lifecycle events.
func TestDeliveryReportStatusMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		reason  string
		want    string // event or "" for acknowledged-only
		detail  string
		encoded func(status, reason, id string) []byte
	}{
		{name: "Success JSON", status: "Success", want: "message.delivered"},
		{name: "Delivered JSON", status: "Delivered", want: "message.delivered"},
		{name: "Failed with reason", status: "Failed", reason: "GatewayRejected", want: "message.failed", detail: "GatewayRejected"},
		{name: "Failed without reason falls back to status", status: "Expired", want: "message.failed", detail: "Expired"},
		{name: "Rejected terminal", status: "Rejected", want: "message.failed", detail: "Rejected"},
		{name: "Discarded terminal", status: "Discarded", want: "message.failed", detail: "Discarded"},
		{name: "Undelivered terminal", status: "Undelivered", want: "message.failed", detail: "Undelivered"},
		{name: "intermediate Enroute acknowledged silently", status: "Enroute", want: ""},
		{name: "intermediate Sent acknowledged silently", status: "Sent", want: ""},
		{
			name:   "form-encoded Success",
			status: "Success",
			want:   "message.delivered",
			encoded: func(status, reason, id string) []byte {
				return []byte("id=" + id + "&status=" + status)
			},
		},
		{name: "unknown status acknowledged, never guessed", status: "WeirdCarrierThing", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAT(t)
			f.recipStatus, f.recipStatusText = 101, "Sent"
			rec := conformance.NewRecorder()
			ad := buildAdapter(t, f, rec, nil)

			msg := validMsg()
			seedInteraction(rec, msg)
			if err := ad.Send(context.Background(), msg); err != nil {
				t.Fatalf("Send: %v", err)
			}

			id := f.messageID(0)
			body := []byte(fmt.Sprintf(`{"id":%q,"status":%q,"failureReason":%q}`, id, tc.status, tc.reason))
			ct := "application/json"
			if tc.encoded != nil {
				body = tc.encoded(tc.status, tc.reason, id)
				ct = "application/x-www-form-urlencoded"
			}
			if err := ad.TranslateDelivery(context.Background(), ct, body); err != nil {
				t.Fatalf("TranslateDelivery: %v", err)
			}

			evs := rec.AppliedFor(testTenant, msg.InteractionID)
			if tc.want == "" {
				if len(evs) != 1 {
					t.Fatalf("intermediate status must not emit lifecycle events, got %v", evs)
				}
				return
			}
			if len(evs) != 2 || evs[1].Event != tc.want {
				t.Fatalf("want second event %q, got %v", tc.want, evs)
			}
			if evs[1].Detail != tc.detail {
				t.Fatalf("detail must be %q, got %q", tc.detail, evs[1].Detail)
			}
			if tc.want == "message.failed" {
				if st, _ := rec.Status(testTenant, msg.InteractionID); string(st) != "failed" {
					t.Fatalf("message.failed must fail the interaction, got %v", st)
				}
			}
		})
	}
}

// TestDeliveryReportGuards pins the failure taxonomy: unknown carrier
// message ids are not-found (no phantom state), malformed/empty reports are
// invalid.
func TestDeliveryReportGuards(t *testing.T) {
	f := newFakeAT(t)
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)
	ctx := context.Background()

	msg := validMsg()
	if err := ad.Send(ctx, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	d0 := len(rec.Deliveries())

	err := ad.TranslateDelivery(ctx, "application/json", []byte(`{"id":"ATUnknownId","status":"Success"}`))
	requireCode(t, err, "at.unknown_message_id")

	err = ad.TranslateDelivery(ctx, "application/json", []byte(`{"status":"Success"}`))
	requireCode(t, err, "at.malformed_callback")

	err = ad.TranslateDelivery(ctx, "application/json", []byte(`{"id":"`+f.messageID(0)+`"}`))
	requireCode(t, err, "at.malformed_callback")

	err = ad.TranslateDelivery(ctx, "application/json", []byte(`{not json at all`))
	requireCode(t, err, "at.malformed_callback")

	err = ad.TranslateDelivery(ctx, "application/x-www-form-urlencoded", []byte("%zz=not-query"))
	requireCode(t, err, "at.malformed_callback")

	if got := len(rec.Deliveries()); got != d0 {
		t.Fatalf("guarded reports must not emit anything, got %d new deliveries", got-d0)
	}
}

// TestSendRetryBudget pins the bounded retry behavior: 429 and 5xx are
// retried until the budget is exhausted, other 4xx fail immediately, a
// transient outage followed by success delivers the full lifecycle, and a
// carrier Retry-After is honored within the configured cap.
func TestSendRetryBudget(t *testing.T) {
	t.Run("429 then success", func(t *testing.T) {
		f := newFakeAT(t)
		f.setScript(func(call int) (int, string) {
			if call == 0 {
				return http.StatusTooManyRequests, `{"SMSMessageData":{"Recipients":[]}}`
			}
			return http.StatusOK, carrierResponse(102, "Delivered", "ATRetryOK")
		})
		rec := conformance.NewRecorder()
		ad := buildAdapter(t, f, rec, nil)

		msg := validMsg()
		seedInteraction(rec, msg)
		if err := ad.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send must succeed after one 429 retry, got: %v", err)
		}
		if got := len(f.recorded()); got != 2 {
			t.Fatalf("exactly 2 carrier attempts expected, got %d", got)
		}
		if evs := rec.AppliedFor(testTenant, msg.InteractionID); len(evs) != 2 ||
			evs[0].Event != "message.sent" || evs[1].Event != "message.delivered" {
			t.Fatalf("post-retry lifecycle must be sent→delivered, got %v", evs)
		}
	})

	t.Run("5xx exhausts the budget", func(t *testing.T) {
		f := newFakeAT(t)
		f.setScript(func(int) (int, string) {
			return http.StatusInternalServerError, "overloaded"
		})
		rec := conformance.NewRecorder()
		ad := buildAdapter(t, f, rec, func(c *africastalking.Config) {
			c.BaseDelay, c.MaxDelay = time.Millisecond, 2*time.Millisecond
		})

		msg := validMsg()
		err := ad.Send(context.Background(), msg)
		var ae *apperrors.Error
		if !errors.As(err, &ae) || ae.Kind != apperrors.KindInternal {
			t.Fatalf("exhausted retries must surface an internal application error, got %v", err)
		}
		if got := len(f.recorded()); got != 3 { // 1 attempt + default 2 retries
			t.Fatalf("bounded budget = 3 attempts, got %d", got)
		}
		if got := len(rec.Deliveries()); got != 0 {
			t.Fatalf("a failed send must not emit receipts, got %d", got)
		}
	})

	t.Run("4xx fails immediately", func(t *testing.T) {
		f := newFakeAT(t)
		f.setScript(func(int) (int, string) {
			return http.StatusBadRequest, `{"error":"bad request"}`
		})
		rec := conformance.NewRecorder()
		ad := buildAdapter(t, f, rec, nil)

		msg := validMsg()
		err := ad.Send(context.Background(), msg)
		var ae *apperrors.Error
		if !errors.As(err, &ae) || ae.Kind != apperrors.KindInvalid {
			t.Fatalf("a 400 must surface invalid, got %v", err)
		}
		if got := len(f.recorded()); got != 1 {
			t.Fatalf("non-retryable 4xx must not retry, got %d attempts", got)
		}
	})

	t.Run("Retry-After honored but capped", func(t *testing.T) {
		f := newFakeAT(t)
		f.retryAfterHeader = "3600" // carrier asks for an hour; the cap must win
		f.setScript(func(call int) (int, string) {
			if call == 0 {
				return http.StatusTooManyRequests, `{}`
			}
			return http.StatusOK, carrierResponse(102, "Delivered", "ATRetryAfterOK")
		})
		rec := conformance.NewRecorder()
		ad := buildAdapter(t, f, rec, func(c *africastalking.Config) {
			c.BaseDelay, c.MaxDelay = time.Millisecond, 10*time.Millisecond
		})

		start := time.Now()
		msg := validMsg()
		if err := ad.Send(context.Background(), msg); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Retry-After must be capped at MaxDelay, waited %s", elapsed)
		}
		if got := len(f.recorded()); got != 2 {
			t.Fatalf("expected a retry after the capped backoff, got %d attempts", got)
		}
	})
}

// TestSendIdempotencySuppressesRepost pins the duplicate-send contract: the
// repeated InteractionID never errors and never re-POSTs an accepted send.
func TestSendIdempotencySuppressesRepost(t *testing.T) {
	f := newFakeAT(t)
	f.recipStatus, f.recipStatusText = 101, "Sent"
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)

	msg := validMsg()
	seedInteraction(rec, msg)
	if err := ad.Send(context.Background(), msg); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := ad.Send(context.Background(), msg); err != nil {
		t.Fatalf("duplicate Send must be accepted (Simulator semantics), got: %v", err)
	}
	if got := len(f.recorded()); got != 1 {
		t.Fatalf("an accepted send must never be re-POSTed, got %d carrier calls", got)
	}
	if evs := rec.AppliedFor(testTenant, msg.InteractionID); len(evs) != 1 {
		t.Fatalf("duplicate send must not re-emit receipts, got %v", evs)
	}
}

// TestSendConcurrentDuplicates pins the single-flight contract under -race:
// N concurrent Sends of the SAME InteractionID produce exactly one carrier
// POST and one receipt set, and every caller reports success.
func TestSendConcurrentDuplicates(t *testing.T) {
	f := newFakeAT(t)
	f.recipStatus, f.recipStatusText = 101, "Sent"
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)

	msg := validMsg()
	seedInteraction(rec, msg)

	const callers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures int
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ad.Send(context.Background(), msg); err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if failures != 0 {
		t.Fatalf("every concurrent duplicate Send must succeed, %d failed", failures)
	}
	if got := len(f.recorded()); got != 1 {
		t.Fatalf("concurrent duplicates must collapse to one carrier POST, got %d", got)
	}
	if evs := rec.AppliedFor(testTenant, msg.InteractionID); len(evs) != 1 || evs[0].Event != "message.sent" {
		t.Fatalf("one receipt set expected, got %v", evs)
	}
}

// TestDestinationE164 pins the E.164 reuse contract: the platform's phone
// canonicalization runs first, then the carrier's strict international rule.
func TestDestinationE164(t *testing.T) {
	f := newFakeAT(t)
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)
	ctx := context.Background()

	cases := []struct {
		to   string
		want string // the canonical destination on the wire; "" = rejected
	}{
		{to: "+254711223344", want: "+254711223344"},
		{to: "254711223344", want: "+254711223344"},
		{to: "00 254 711 223344", want: "+254711223344"},
		{to: "+254 711 223 344", want: "+254711223344"},
		{to: "+25471122", want: "+25471122"},  // 8-digit minimum is valid E.164
		{to: "0711223344", want: ""},          // local format canonicalizes with '+0…', not routable
		{to: "+0711223344", want: ""},         // leading zero after '+': not E.164
		{to: "not-a-phone", want: ""},         // garbage
		{to: "+254711223344567890", want: ""}, // over 15 digits
	}
	for _, tc := range cases {
		msg := validMsg()
		msg.To = tc.to
		err := ad.Send(ctx, msg)
		if tc.want == "" {
			if err == nil {
				t.Errorf("destination %q must be rejected", tc.to)
				continue
			}
			var ae *apperrors.Error
			if !errors.As(err, &ae) || ae.Code != "at.to_not_e164" {
				t.Errorf("destination %q must fail with at.to_not_e164, got %v", tc.to, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("destination %q must be accepted, got %v", tc.to, err)
			continue
		}
		if got := f.recorded()[len(f.recorded())-1].To; got != tc.want {
			t.Errorf("destination %q must hit the carrier as %q, got %q", tc.to, tc.want, got)
		}
	}
}

// TestSendContextCancellation: a canceled context fails the send without
// emitting anything.
func TestSendContextCancellation(t *testing.T) {
	f := newFakeAT(t)
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := validMsg()
	if err := ad.Send(ctx, msg); err == nil {
		t.Fatal("Send on a canceled context must fail")
	}
	if got := len(rec.Deliveries()); got != 0 {
		t.Fatalf("a canceled send must not emit receipts, got %d", got)
	}
}

// TestRedactionZeroLeakage is the adversarial credential/PII sweep: across
// happy paths, failure paths and renderings, the API key appears in exactly
// one place — the apiKey request header — and phone numbers never reach
// logs, event payloads or error surfaces.
func TestRedactionZeroLeakage(t *testing.T) {
	var (
		logBuf    strings.Builder
		errs      []error
		eventBods []string
	)
	f := newFakeAT(t)
	f.recipStatus, f.recipStatusText = 101, "Sent"
	f.setScript(func(call int) (int, string) {
		if call == 1 { // a non-retryable carrier rejection on the 2nd send
			return http.StatusUnauthorized, `{"error":"invalid api key"}`
		}
		return f.defaultResponse(call)
	})
	rec := conformance.NewRecorder()
	ad := buildAdapter(t, f, rec, func(c *africastalking.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	})
	ctx := context.Background()

	// 1) Happy send + delivery report (report carries the MSISDN, as the
	// real carrier does — the adapter must not echo it anywhere).
	msg := validMsg()
	seedInteraction(rec, msg)
	if err := ad.Send(ctx, msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := ad.TranslateDelivery(ctx, "application/json",
		[]byte(fmt.Sprintf(`{"id":%q,"status":"Success","phoneNumber":%q}`, f.messageID(0), testMSISDN))); err != nil {
		t.Fatalf("TranslateDelivery: %v", err)
	}

	// 2) Carrier rejection (401) — the error must not echo credentials.
	msg2 := validMsg()
	seedInteraction(rec, msg2)
	errs = append(errs, ad.Send(ctx, msg2))

	// 3) Guarded report paths.
	errs = append(errs,
		ad.TranslateDelivery(ctx, "application/json", []byte(`{"id":"ATUnknown","status":"Success"}`)),
		ad.TranslateDelivery(ctx, "application/json", []byte(`{"status":"Success"}`)),
		ad.TranslateDelivery(ctx, "application/json", []byte(`{broken`)),
	)

	for _, d := range rec.Deliveries() {
		eventBods = append(eventBods, string(d.Body))
	}

	// The one legitimate place: the apiKey header on every carrier POST.
	for i, s := range f.recorded() {
		if s.APIKey != testAPIKey {
			t.Fatalf("send #%d: apiKey header must carry the credential exactly", i)
		}
	}

	// %#v of a Config full of registry.Secret values renders redacted.
	cfgRendering := fmt.Sprintf("%#v", africastalking.Config{
		Username: registry.Secret(testAPIKey),
		APIKey:   registry.Secret(testAPIKey),
		SenderID: registry.Secret(testAPIKey),
	})

	// The API key is hunted across EVERY surface, including the carrier
	// request bodies. The subscriber MSISDN is hunted only across the
	// PLATFORM surfaces (events, logs, errors, renderings): the carrier
	// request body legitimately carries the destination — that is the
	// message being delivered — so asserting its absence there would be
	// fiction. The PII rule is that the adapter never echoes subscriber
	// data into platform surfaces it controls.
	surfaces := map[string]string{
		"carrier request bodies": rawBodies(f),
		"event payloads":         strings.Join(eventBods, "\n"),
		"log output":             logBuf.String(),
		"error surfaces":         errorBlob(errs),
		"secret rendering":       cfgRendering,
	}
	for name, blob := range surfaces {
		if idx := strings.Index(blob, leakMarker); idx >= 0 {
			t.Errorf("credential leak in %s at offset %d", name, idx)
		}
	}
	for name, blob := range map[string]string{
		"event payloads":   surfaces["event payloads"],
		"log output":       surfaces["log output"],
		"error surfaces":   surfaces["error surfaces"],
		"secret rendering": surfaces["secret rendering"],
	} {
		if strings.Contains(blob, testMSISDN) {
			t.Errorf("PII (subscriber MSISDN) leak in %s", name)
		}
	}
}

// rawBodies joins every carrier request body the fake observed.
func rawBodies(f *fakeAT) string {
	var out []string
	for _, s := range f.recorded() {
		out = append(out, s.RawBody)
	}
	return strings.Join(out, "\n")
}

// errorBlob renders every error surface into one searchable string.
func errorBlob(errs []error) string {
	var b strings.Builder
	for _, err := range errs {
		if err == nil {
			continue
		}
		fmt.Fprintf(&b, "%v\n", err)
		var ae *apperrors.Error
		if errors.As(err, &ae) && ae.Details != nil {
			fmt.Fprintf(&b, "%+v\n", ae.Details)
		}
	}
	return b.String()
}
