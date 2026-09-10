package africastalking

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// ─────────────────────────────────────────────────────────────────────────────
// Conformance (the gate: an adapter that does not pass the kit is not
// reviewable — internal/comms/conformance/README.md)
// ─────────────────────────────────────────────────────────────────────────────

// TestATVoiceConformance runs the full voice behavioral contract against the
// adapter in ASYNC mode: the dial is queued by the fake carrier (POST /call),
// and lifecycle events materialize only when the fake's callback pump plays
// the carrier's status callbacks over the real HTTP callback surface
// (adapter.StatusCallbackHandler). RequireAsyncEvents is mandatory for this
// adapter — by contract PlaceCall emits nothing; callbacks speak later.
// EnforceCommandValidation is on because the adapter validates the full
// command before any carrier contact (typed at.* codes).
func TestATVoiceConformance(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	conformance.RunVoiceConformance(t, h.adapter, conformance.Options{
		Recorder:                 rec,
		ProviderName:             ProviderName,
		RequireAsyncEvents:       true,
		EventSettleTimeout:       time.Second,
		EnforceCommandValidation: true,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Dial plane e2e: POST /call → queued → callbacks → lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// TestPlaceCallE2EAsyncLifecycle pins the honest async contract end to end:
// PlaceCall returns after AT queues the dial (nothing has rung), the fake
// carrier's callbacks drive ringing → connected over the real HTTP callback
// path, a terminal callback carries carrier truth (cause, duration,
// provider timestamp) and lands wrapup, and post-terminal control is either
// idempotent (hangup) or the typed ErrUnsupportedOperation — never faked.
func TestPlaceCallE2EAsyncLifecycle(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	ctx := context.Background()
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)

	if err := h.placeCall(ctx, id); err != nil {
		t.Fatalf("PlaceCall must succeed for a valid tenant-bound command, got: %v", err)
	}
	// The dial reached the wire exactly once, authenticated and shaped.
	dials := h.fake.dialRequests()
	if len(dials) != 1 {
		t.Fatalf("exactly one make-call POST must be sent, got %d", len(dials))
	}
	d := dials[0]
	if d.Method != http.MethodPost || d.Path != callPath {
		t.Errorf("dial must be POST %s, got %s %s", callPath, d.Method, d.Path)
	}
	for k, want := range map[string]string{
		"username":        testUsername,
		"to":              "+254711111111",
		"from":            "+254700000001",
		"clientRequestId": id,
	} {
		if got := d.Form.Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}
	// Wire contract (research finding, README): the credential travels in the
	// apikey header. There is NO Authorization/Bearer header — that scheme
	// belongs to other surfaces and is deliberately not faked here.
	if d.APIKeyHdr != testAPIKey {
		t.Errorf("apikey header = %q, want the configured key", d.APIKeyHdr)
	}
	if d.AuthHdr != "" {
		t.Errorf("Authorization header must be absent (AT voice uses apikey), got %q", d.AuthHdr)
	}

	evs := awaitEvents(t, rec, testTenant, id, 2)
	if evs[0].Event != "call.ringing" || evs[1].Event != "call.connected" {
		t.Fatalf("async lifecycle must be ringing then connected, got %s then %s", evs[0].Event, evs[1].Event)
	}
	for _, ev := range evs {
		if ev.InteractionID != id || ev.TenantID != testTenant {
			t.Errorf("lifecycle event %s must carry interaction=%s tenant=%s", ev.Event, id, testTenant)
		}
	}
	for i, dl := range rec.Deliveries() {
		if dl.Signature == "" {
			t.Errorf("delivery #%d must be signed (fail-closed gateway)", i)
		}
		if dl.Provider != ProviderName {
			t.Errorf("delivery #%d provider = %q, want %q", i, dl.Provider, ProviderName)
		}
	}
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusActive {
		t.Fatalf("interaction must be active after connect, got %s", st)
	}

	// Carrier truth: a terminal callback with cause, duration and provider
	// timestamp. The event Detail is the end_reason audit trail.
	sid, crid := h.fake.lastLeg()
	x := h.fake.deliverCallback(StatusCallback{
		SessionID:       sid,
		ClientRequestID: crid,
		Status:          StatusCompleted,
		HangupCause:     "NORMAL_CLEARING",
		DurationSecs:    42,
		Timestamp:       "2026-09-01T10:00:00Z",
	})
	if x.StatusCode != http.StatusOK {
		t.Fatalf("terminal callback must be accepted, got %d (%s)", x.StatusCode, x.Response)
	}
	evs = awaitEvents(t, rec, testTenant, id, 3)
	ended := evs[2]
	if ended.Event != "call.ended" {
		t.Fatalf("terminal callback must land call.ended, got %s", ended.Event)
	}
	if ended.Detail != "completed (NORMAL_CLEARING)" {
		t.Errorf("call.ended Detail = %q, want %q (reason + carrier cause)", ended.Detail, "completed (NORMAL_CLEARING)")
	}
	if ended.Timestamp != "2026-09-01T10:00:00Z" {
		t.Errorf("provider timestamp must pass through, got %q", ended.Timestamp)
	}
	if st, _ := rec.Status(testTenant, id); st != interactions.StatusWrapup {
		t.Fatalf("terminal callback must land wrapup, got %s", st)
	}

	// Post-terminal control (capability matrix): hangup is idempotent (the
	// requested end-state factually holds and must not re-emit), everything
	// else that would act on a terminated session is the typed unsupported
	// error — AT exposes no API for terminated sessions.
	if err := h.adapter.Hangup(ctx, id); err != nil {
		t.Errorf("hangup on an ended leg must be an idempotent no-op, got: %v", err)
	}
	for name, op := range map[string]func() error{
		"Transfer": func() error { return h.adapter.Transfer(ctx, id, "+254722000000") },
		"Hold":     func() error { return h.adapter.Hold(ctx, id) },
		"Resume":   func() error { return h.adapter.Resume(ctx, id) },
	} {
		if err := op(); !errors.Is(err, ErrUnsupportedOperation) {
			t.Errorf("%s on a terminal leg must be ErrUnsupportedOperation, got: %v", name, err)
		}
	}
	if err := h.adapter.Hangup(ctx, "no-such-leg"); err == nil {
		t.Errorf("hangup on an unknown leg must fail loudly")
	}
	awaitStable(t, rec, 3, "post-terminal silence")
}

// TestPlaceCallRejectsDuplicateInteraction pins the double-bill guard: a
// second dial for a live interaction is a conflict BEFORE any carrier
// contact — a second real POST would bill the customer twice.
func TestPlaceCallRejectsDuplicateInteraction(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	if err := h.placeCall(context.Background(), id); err != nil {
		t.Fatalf("first PlaceCall: %v", err)
	}
	err := h.placeCall(context.Background(), id)
	expectAppError(t, err, apperrors.KindConflict, "at.interaction_already_placed", "duplicate interaction")
	if got := len(h.fake.dialRequests()); got != 1 {
		t.Errorf("duplicate interaction must not reach the carrier, got %d dials", got)
	}
}

// TestPlaceCallRejectsInvalidPhone pins the E.164 gate: malformed
// destinations are rejected before any carrier contact.
func TestPlaceCallRejectsInvalidPhone(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	err := h.adapter.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "not-a-phone",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	})
	expectAppError(t, err, apperrors.KindInvalid, "at.invalid_phone", "invalid destination")
	if got := len(h.fake.dialRequests()); got != 0 {
		t.Errorf("rejected destination must not reach the carrier, got %d dials", got)
	}
}

// TestPlaceCallSingleEntryFallback pins the envelope matching rule: when AT
// does not echo the dialed number, the single entry is still selected — a
// 2xx queued envelope must never be dropped as unreadable.
func TestPlaceCallSingleEntryFallback(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, nil)
	f.setResponse(func(_ url.Values) (int, http.Header, string) {
		return http.StatusOK, nil, `{"errorMessage":"None","entries":[{"phoneNumber":"+999000000","sessionId":"ATVerb_x1","status":"Queued","errorMessage":"None","clientRequestId":"lost"}]}`
	})
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	if err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	}); err != nil {
		t.Fatalf("single-entry envelope must be accepted, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Error taxonomy: every upstream shape maps to one typed *apperrors.Error
// ─────────────────────────────────────────────────────────────────────────────

// TestPlaceCallErrorTable is the carrier-failure table: each upstream shape
// maps to exactly one kind + stable machine code, retries happen only for
// 409/429/5xx (the bounded budget), and a rejected dial emits NOTHING
// (no deliveries, no failed deliveries — the core owns the retry story).
func TestPlaceCallErrorTable(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantKind     apperrors.Kind
		wantCode     string
		wantAttempts int
		wantDetails  bool // status-driven errors carry bounded details
	}{
		{"400 request rejected", http.StatusBadRequest, `{"errorMessage":"Malformed request"}`, apperrors.KindInvalid, "at.request_rejected", 1, true},
		{"401 auth failed", http.StatusUnauthorized, `{"errorMessage":"Invalid api key"}`, apperrors.KindUnauth, "at.auth_failed", 1, true},
		{"403 forbidden", http.StatusForbidden, `{"errorMessage":"Forbidden"}`, apperrors.KindForbidden, "at.forbidden", 1, true},
		{"404 not found", http.StatusNotFound, `{"errorMessage":"Not Found"}`, apperrors.KindNotFound, "at.not_found", 1, true},
		{"409 conflict retried", http.StatusConflict, `{"errorMessage":"Conflict"}`, apperrors.KindConflict, "at.conflict", 4, true},
		{"429 rate limited retried", http.StatusTooManyRequests, `{"errorMessage":"Rate limited"}`, apperrors.KindRateLimited, "at.rate_limited", 4, true},
		{"500 upstream retried", http.StatusInternalServerError, `{"errorMessage":"Boom"}`, apperrors.KindInternal, "at.upstream_error", 4, true},
		{"200 envelope-level rejection", http.StatusOK, `{"errorMessage":"Insufficient balance","entries":[]}`, apperrors.KindConflict, "at.call_rejected", 1, true},
		{"200 entry not queued", http.StatusOK, `{"errorMessage":"None","entries":[{"phoneNumber":"+254711111111","sessionId":"ATVerb_x2","status":"Failed","errorMessage":"Destination not reachable"}]}`, apperrors.KindConflict, "at.call_not_queued", 1, true},
		{"200 entry error while queued", http.StatusOK, `{"errorMessage":"None","entries":[{"phoneNumber":"+254711111111","sessionId":"ATVerb_x3","status":"Queued","errorMessage":"Insufficient balance"}]}`, apperrors.KindConflict, "at.call_not_queued", 1, true},
		{"200 unreadable envelope", http.StatusOK, `not-json`, apperrors.KindInternal, "at.envelope_invalid", 1, false},
		{"200 no entry for destination", http.StatusOK, `{"errorMessage":"None","entries":[]}`, apperrors.KindInternal, "at.envelope_invalid", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := conformance.NewRecorder()
			f := newFakeAT(t)
			a := newTestAdapter(t, f, rec.Ingest, nil)
			a.sleep = func(time.Duration) {} // deterministic: no real backoff waits
			a.jitterFn = func(d time.Duration) time.Duration { return 0 }
			f.setResponse(func(_ url.Values) (int, http.Header, string) {
				return tc.status, nil, tc.body
			})
			id := uuid.NewString()
			rec.Seed(testTenant, id, interactions.StatusPending)

			err := a.PlaceCall(context.Background(), telephony.CallCommand{
				InteractionID:   id,
				From:            "+254700000001",
				To:              "+254711111111",
				ProviderOptions: map[string]any{"tenant_id": testTenant},
			})
			ae := expectAppError(t, err, tc.wantKind, tc.wantCode, tc.name)
			if ae != nil && tc.wantDetails && ae.Details == nil {
				t.Errorf("%s: error must carry bounded details (http_status / at_message)", tc.name)
			}
			if got := len(f.dialRequests()); got != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d (retries only on 409/429/5xx, bounded ≤%d)", got, tc.wantAttempts, maxRetriesCeiling)
			}
			// Invariant: a failed dial is silent — the core's provider-failure
			// path owns what happens next; the adapter must not emit events.
			awaitStable(t, rec, 0, tc.name+": failed dial emits nothing")
			// Invariant: provider text is bounded into details, never the
			// credential material or endpoint into the message.
			assertNoCredentialMaterial(t, err, ae)
		})
	}
}

// TestWrongAPIKeyRejectedByFake proves the fake's auth gate and the adapter's
// 401 mapping together: a misconfigured credential is a typed auth failure,
// never a queued dial and never a retry.
func TestWrongAPIKeyRejectedByFake(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, func(cfg *Config) { cfg.APIKey = "wrong-key" })
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	})
	expectAppError(t, err, apperrors.KindUnauth, "at.auth_failed", "wrong api key")
	awaitStable(t, rec, 0, "auth failure emits nothing")
}

// ─────────────────────────────────────────────────────────────────────────────
// Bounded retry posture
// ─────────────────────────────────────────────────────────────────────────────

// TestPlaceCallRetriesThenSucceeds pins the bounded-retry contract on the
// happy path: 429 → 409 → 500 → queued. Transient congestion must not fail
// real interactions.
func TestPlaceCallRetriesThenSucceeds(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, nil)
	a.sleep = func(time.Duration) {}
	failures := []string{
		`{"errorMessage":"Rate limited"}`,
		`{"errorMessage":"Conflict"}`,
		`{"errorMessage":"Boom"}`,
	}
	idx := 0
	f.setResponse(func(form url.Values) (int, http.Header, string) {
		if idx < len(failures) {
			s := failures[idx]
			idx++
			return []int{http.StatusTooManyRequests, http.StatusConflict, http.StatusInternalServerError}[idx-1], nil, s
		}
		return f.defaultQueued(form)
	})
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	if err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	}); err != nil {
		t.Fatalf("PlaceCall after transient failures must succeed, got: %v", err)
	}
	if got := len(f.dialRequests()); got != 4 {
		t.Errorf("attempts = %d, want 4 (three transient failures + success)", got)
	}
}

// TestPlaceCallHonorsRetryAfter pins that server-provided Retry-After
// guidance is honored EXACTLY (no jitter on top) — the polite-saturation
// contract for AT rate limiting.
func TestPlaceCallHonorsRetryAfter(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, nil)
	var sleeps []time.Duration
	var jitterCalled bool
	a.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	a.jitterFn = func(d time.Duration) time.Duration { jitterCalled = true; return d }

	hdr := http.Header{"Retry-After": []string{"1"}}
	attempts := 0
	f.setResponse(func(form url.Values) (int, http.Header, string) {
		attempts++
		if attempts <= 3 {
			return http.StatusTooManyRequests, hdr, `{"errorMessage":"Rate limited"}`
		}
		return f.defaultQueued(form)
	})
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	if err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	}); err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	want := []time.Duration{time.Second, time.Second, time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want exactly %v (Retry-After honored per retry)", sleeps, want)
	}
	for i, d := range want {
		if sleeps[i] != d {
			t.Errorf("sleep[%d] = %s, want %s (server guidance honored exactly, never jittered)", i, sleeps[i], d)
		}
	}
	if jitterCalled {
		t.Errorf("jitter must not run while server guidance is present")
	}
}

// TestPlaceCallExponentialJitterSchedule pins the no-guidance backoff shape:
// full-jitter under an exponential ceiling (base, base<<1, base<<2) with the
// configured base. Deterministic via the identity jitter seam.
func TestPlaceCallExponentialJitterSchedule(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, func(cfg *Config) { cfg.RetryBackoff = 10 * time.Millisecond })
	var sleeps []time.Duration
	a.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	a.jitterFn = func(d time.Duration) time.Duration { return d } // identity: ceiling pick

	attempts := 0
	f.setResponse(func(form url.Values) (int, http.Header, string) {
		attempts++
		if attempts <= 3 {
			return http.StatusTooManyRequests, nil, `{"errorMessage":"Rate limited"}`
		}
		return f.defaultQueued(form)
	})
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	if err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	}); err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i, d := range want {
		if sleeps[i] != d {
			t.Errorf("sleep[%d] = %s, want %s (exponential base<<attempt)", i, sleeps[i], d)
		}
	}
}

// TestPlaceCallContextCancellation pins cancellation semantics: a cancelled
// context stops the retry loop without further carrier contact, and the
// surfaced error is the context error (not an invented apperrors).
func TestPlaceCallContextCancellation(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, nil)
	a.sleep = func(time.Duration) {}
	f.setResponse(func(_ url.Values) (int, http.Header, string) {
		return http.StatusTooManyRequests, nil, `{"errorMessage":"Rate limited"}`
	})
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)

	t.Run("cancelled mid-retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		a.sleep = func(time.Duration) { cancel() } // cancel during the first backoff pause
		err := a.PlaceCall(ctx, telephony.CallCommand{
			InteractionID:   id,
			From:            "+254700000001",
			To:              "+254711111111",
			ProviderOptions: map[string]any{"tenant_id": testTenant},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
		if got := len(f.dialRequests()); got != 1 {
			t.Errorf("cancelled retry loop must stop dialing, got %d attempts", got)
		}
	})

	t.Run("pre-cancelled", func(t *testing.T) {
		before := len(f.dialRequests())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := a.PlaceCall(ctx, telephony.CallCommand{
			InteractionID:   id,
			From:            "+254700000001",
			To:              "+254711111111",
			ProviderOptions: map[string]any{"tenant_id": testTenant},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
		if got := len(f.dialRequests()); got != before {
			t.Errorf("pre-cancelled context must not dial, got %d new attempts", got-before)
		}
	})
}

// TestPlaceCallTransportError pins the transport-failure mapping: a dead
// upstream is at.transport_error (internal), the URL is sanitized (bounded),
// and no credential material rides the chain.
func TestPlaceCallTransportError(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	base := dead.URL
	dead.Close() // nothing listens here anymore

	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, func(cfg *Config) { cfg.APIBaseURL = base })
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	})
	expectAppError(t, err, apperrors.KindInternal, "at.transport_error", "dead upstream")
	awaitStable(t, rec, 0, "transport failure emits nothing")
	assertNoCredentialMaterial(t, err, nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// Callback plane: translation table + routing + handler wire behavior
// ─────────────────────────────────────────────────────────────────────────────

// TestTranslateStatusTable is the closed-vocabulary contract (pure helper):
// each AT status maps to exactly one ProviderEvent, terminal causes all land
// call.ended with the reason (plus the carrier cause) as Detail, and an
// unknown status is a typed error — never a guessed event.
func TestTranslateStatusTable(t *testing.T) {
	const (
		interaction = "019393a0-1000-7000-8000-00000000beef"
		tenant      = testTenant
	)
	cases := []struct {
		name       string
		cb         StatusCallback
		wantEvent  string
		wantDetail string
	}{
		{"queued rings", StatusCallback{Status: StatusQueued}, "call.ringing", ""},
		{"in progress connects", StatusCallback{Status: StatusInProgress}, "call.connected", ""},
		{"completed", StatusCallback{Status: StatusCompleted}, "call.ended", "completed"},
		{"completed with cause", StatusCallback{Status: StatusCompleted, HangupCause: "NORMAL_CLEARING"}, "call.ended", "completed (NORMAL_CLEARING)"},
		{"failed", StatusCallback{Status: StatusFailed}, "call.ended", "failed"},
		{"busy", StatusCallback{Status: StatusBusy}, "call.ended", "busy"},
		{"busy does not duplicate cause", StatusCallback{Status: StatusBusy, HangupCause: "busy"}, "call.ended", "busy"},
		{"no answer with cause", StatusCallback{Status: StatusNoAnswer, HangupCause: "NO_ANSWER"}, "call.ended", "no_answer (NO_ANSWER)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := TranslateStatus(&tc.cb, interaction, tenant)
			if err != nil {
				t.Fatalf("TranslateStatus: %v", err)
			}
			if ev.Event != tc.wantEvent {
				t.Errorf("event = %q, want %q", ev.Event, tc.wantEvent)
			}
			if ev.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", ev.Detail, tc.wantDetail)
			}
			if ev.InteractionID != interaction || ev.TenantID != tenant {
				t.Errorf("routing keys must pass through: interaction=%q tenant=%q", ev.InteractionID, ev.TenantID)
			}
		})
	}

	t.Run("provider timestamp passes through when parseable", func(t *testing.T) {
		ev, err := TranslateStatus(&StatusCallback{Status: StatusQueued, Timestamp: "2026-09-01T10:00:00Z"}, interaction, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Timestamp != "2026-09-01T10:00:00Z" {
			t.Errorf("timestamp = %q, want the provider stamp", ev.Timestamp)
		}
	})
	t.Run("unparseable timestamp is dropped, not propagated", func(t *testing.T) {
		ev, err := TranslateStatus(&StatusCallback{Status: StatusQueued, Timestamp: "yesterday-ish"}, interaction, tenant)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Timestamp != "" {
			t.Errorf("timestamp = %q, want empty (platform clock stamps the event)", ev.Timestamp)
		}
	})

	errCases := []struct {
		name     string
		cb       *StatusCallback
		id       string
		tenant   string
		wantKind apperrors.Kind
		wantCode string
	}{
		{"nil callback", nil, interaction, tenant, apperrors.KindInvalid, "at.callback_malformed"},
		{"unknown status", &StatusCallback{Status: "Teleported"}, interaction, tenant, apperrors.KindInvalid, "at.unknown_status"},
		{"unroutable: no interaction", &StatusCallback{Status: StatusQueued}, "", tenant, apperrors.KindNotFound, "at.callback_unroutable"},
		{"unroutable: no tenant", &StatusCallback{Status: StatusQueued}, interaction, "", apperrors.KindNotFound, "at.callback_unroutable"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TranslateStatus(tc.cb, tc.id, tc.tenant)
			expectAppError(t, err, tc.wantKind, tc.wantCode, tc.name)
		})
	}
}

// TestStatusCallbacksE2E drives the callback surface over real HTTP (the
// StatusCallbackHandler the gateway mounts) and pins the routing and wire
// behavior: clientRequestId echo wins, session registry routes the rest,
// phantom callbacks are rejected not guessed, unknown statuses are typed
// errors, and terminal callbacks mark the leg for honest post-terminal
// control semantics.
func TestStatusCallbacksE2E(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)

	// Manual control: the auto pump stays silent; the test plays the carrier.
	h.fake.setCallbacks(func(_, _ string) []StatusCallback { return nil })
	if err := h.placeCall(context.Background(), id); err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	sid, crid := h.fake.lastLeg()
	if sid == "" || crid != id {
		t.Fatalf("fake must mint a sessionId and echo clientRequestId=%s, got sid=%q crid=%q", id, sid, crid)
	}

	t.Run("clientRequestId echo routes the queued callback", func(t *testing.T) {
		x := h.fake.deliverCallback(StatusCallback{ClientRequestID: crid, Status: StatusQueued})
		if x.StatusCode != http.StatusOK {
			t.Fatalf("queued callback must be accepted, got %d (%s)", x.StatusCode, x.Response)
		}
		evs := awaitEvents(t, rec, testTenant, id, 1)
		if evs[0].Event != "call.ringing" {
			t.Errorf("event = %s, want call.ringing", evs[0].Event)
		}
	})

	t.Run("sessionId routes when the echo is absent", func(t *testing.T) {
		x := h.fake.deliverCallback(StatusCallback{SessionID: sid, Status: StatusInProgress, Timestamp: "not-a-time"})
		if x.StatusCode != http.StatusOK {
			t.Fatalf("in-progress callback must be accepted, got %d (%s)", x.StatusCode, x.Response)
		}
		evs := awaitEvents(t, rec, testTenant, id, 2)
		if evs[1].Event != "call.connected" {
			t.Errorf("event = %s, want call.connected", evs[1].Event)
		}
		if evs[1].Timestamp == "not-a-time" {
			t.Errorf("unparseable provider timestamp must be dropped, got %q", evs[1].Timestamp)
		}
		if evs[1].Timestamp == "" {
			t.Errorf("event must carry the platform-stamped timestamp")
		}
	})

	t.Run("unknown status is a typed 422, not a guessed event", func(t *testing.T) {
		x := h.fake.deliverCallback(StatusCallback{SessionID: sid, Status: "Teleported"})
		if x.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", x.StatusCode)
		}
	})

	t.Run("phantom callback is 404 — never materialized", func(t *testing.T) {
		x := h.fake.deliverCallback(StatusCallback{SessionID: "ATVerb_phantom", Status: StatusCompleted})
		if x.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (unroutable callbacks must not become events)", x.StatusCode)
		}
	})

	t.Run("malformed JSON is a typed 422", func(t *testing.T) {
		resp, err := http.Post(h.fake.cbURLNow(), "application/json", strings.NewReader("{not json"))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", resp.StatusCode)
		}
	})

	t.Run("terminal busy callback lands wrapup with the carrier cause", func(t *testing.T) {
		x := h.fake.deliverCallback(StatusCallback{SessionID: sid, Status: StatusBusy, HangupCause: "USER_BUSY"})
		if x.StatusCode != http.StatusOK {
			t.Fatalf("terminal callback must be accepted, got %d (%s)", x.StatusCode, x.Response)
		}
		evs := awaitEvents(t, rec, testTenant, id, 3)
		last := evs[2]
		if last.Event != "call.ended" || last.Detail != "busy (USER_BUSY)" {
			t.Errorf("terminal event = %s detail=%q, want call.ended %q", last.Event, last.Detail, "busy (USER_BUSY)")
		}
		if st, _ := rec.Status(testTenant, id); st != interactions.StatusWrapup {
			t.Errorf("status = %s, want wrapup", st)
		}
	})

	t.Run("post-terminal: hangup idempotent, control typed-unsupported, silence holds", func(t *testing.T) {
		ctx := context.Background()
		if err := h.adapter.Hangup(ctx, id); err != nil {
			t.Errorf("hangup on ended leg must be a no-op, got: %v", err)
		}
		if err := h.adapter.Hold(ctx, id); !errors.Is(err, ErrUnsupportedOperation) {
			t.Errorf("hold on ended leg must be ErrUnsupportedOperation, got: %v", err)
		}
		if err := h.adapter.Resume(ctx, id); !errors.Is(err, ErrUnsupportedOperation) {
			t.Errorf("resume on ended leg must be ErrUnsupportedOperation, got: %v", err)
		}
		if err := h.adapter.Transfer(ctx, id, "+254722000000"); !errors.Is(err, ErrUnsupportedOperation) {
			t.Errorf("transfer on ended leg must be ErrUnsupportedOperation, got: %v", err)
		}
		expectAppError(t, h.adapter.Hangup(ctx, "never-placed"),
			apperrors.KindNotFound, "at.unknown_leg", "hangup on unknown leg")
		awaitStable(t, rec, 3, "post-terminal silence")
	})
}

// TestInCallControlRidesCallbackResponse pins AT's in-call control channel:
// with no REST control surface, the queued carrier-side action must ride the
// NEXT callback response as callback-action XML — transfer as <Dial>, hangup
// as the deliberate empty <Response> (AT ends the leg when its action queue
// runs out). The exchange is captured by the fake as wire proof.
func TestInCallControlRidesCallbackResponse(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	ctx := context.Background()
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	if err := h.placeCall(ctx, id); err != nil {
		t.Fatalf("PlaceCall: %v", err)
	}
	awaitEvents(t, rec, testTenant, id, 2) // pump: queued + in progress
	if n := len(successfulExchanges(h.fake)); n != 2 {
		t.Fatalf("pump must have delivered exactly two accepted callbacks, got %d (exchanges may include routing retries)", n)
	}
	sid, crid := h.fake.lastLeg()

	// Transfer: audit ringing event + queued <Dial> action.
	if err := h.adapter.Transfer(ctx, id, "+254722000000"); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	evs := awaitEvents(t, rec, testTenant, id, 3)
	if evs[2].Event != "call.ringing" || !strings.Contains(evs[2].Detail, "+254722000000") {
		t.Errorf("transfer must re-ring with the destination in Detail, got %s/%q", evs[2].Event, evs[2].Detail)
	}
	x := h.fake.deliverCallback(StatusCallback{SessionID: sid, ClientRequestID: crid, Status: StatusInProgress})
	if x.StatusCode != http.StatusOK {
		t.Fatalf("callback must be accepted, got %d (%s)", x.StatusCode, x.Response)
	}
	if got, want := string(x.Response), `<Dial phoneNumbers="+254722000000"/>`; !strings.Contains(got, want) {
		t.Errorf("transfer action must ride the callback response as %s, got %q", want, got)
	}

	// Hangup: ended event + the deliberate empty action body (no further
	// actions → AT ends the leg when its session action queue drains).
	if err := h.adapter.Hangup(ctx, id); err != nil {
		t.Fatalf("Hangup: %v", err)
	}
	awaitEvents(t, rec, testTenant, id, 4)
	x = h.fake.deliverCallback(StatusCallback{SessionID: sid, Status: StatusCompleted, HangupCause: "NORMAL_CLEARING"})
	if x.StatusCode != http.StatusOK {
		t.Fatalf("terminal callback must be accepted, got %d (%s)", x.StatusCode, x.Response)
	}
	if got, want := string(x.Response), renderAction(nil); got != want {
		t.Errorf("hangup action body = %q, want %q (empty <Response> = AT ends the leg)", got, want)
	}
}

// TestOversizedCallbackBodyIsRejected pins the memory bound: a body beyond
// callbackBodyLimit is truncated on read, which cannot parse as JSON, and is
// rejected as a typed invalid error (422) — a hostile caller cannot balloon
// adapter memory.
func TestOversizedCallbackBodyIsRejected(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	big := "{" + strings.Repeat("a", callbackBodyLimit*2) + "}"
	resp, err := http.Post(h.fake.cbURLNow(), "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("oversized body status = %d, want 422", resp.StatusCode)
	}
	if got := len(rec.Deliveries()); got != 0 {
		t.Errorf("oversized body must not become a delivery, got %d", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Redaction: zero credential material on every rendering path
// ─────────────────────────────────────────────────────────────────────────────

// TestAdapterRedactionEveryPath is the adversarial leakage matrix for the
// adapter: every top-level fmt verb, Stringer, GoStringer, slog LogValuer and
// encoding/json path must render masked credentials only. The adapter renders
// ITSELF redacted because fmt refuses Stringer calls on values reached
// through unexported fields (see Adapter.String).
func TestAdapterRedactionEveryPath(t *testing.T) {
	rec := conformance.NewRecorder()
	h := newHarness(t, rec.Ingest, nil)
	a := h.adapter

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	logger.Info("adapter", "value", a)

	renderings := map[string]string{
		"fmt %v":     fmt.Sprintf("%v", a),
		"fmt %+v":    fmt.Sprintf("%+v", a),
		"fmt %#v":    fmt.Sprintf("%#v", a),
		"String()":   a.String(),
		"GoString()": a.GoString(),
		"slog":       logBuf.String(),
		"json":       string(mustJSON(t, a)),
	}
	for name, out := range renderings {
		for _, raw := range []string{testUsername, testAPIKey} {
			if strings.Contains(out, raw) {
				t.Errorf("%s leaked credential material %q: %s", name, raw, out)
			}
		}
		if !strings.Contains(out, "****") {
			t.Errorf("%s must render the masked credential form, got %q", name, out)
		}
	}
	if !strings.Contains(renderings["String()"], "africastalking.Adapter(") {
		t.Errorf("String() must keep the stable prefix, got %q", renderings["String()"])
	}
	if !strings.Contains(renderings["json"], a.apiBase) {
		t.Errorf("json must keep the non-credential api_base, got %s", renderings["json"])
	}
}

// TestConfigRedaction pins the configuration struct: exported Secret fields
// self-render masked on fmt and json paths (the Config is passed around by
// value, unlike the Adapter).
func TestConfigRedaction(t *testing.T) {
	cfg := Config{Username: testUsername, APIKey: testAPIKey, APIBaseURL: "https://voice.example"}
	renderings := map[string]string{
		"fmt %v":  fmt.Sprintf("%v", cfg),
		"fmt %+v": fmt.Sprintf("%+v", cfg),
		"fmt %#v": fmt.Sprintf("%#v", cfg),
		"json":    string(mustJSON(t, cfg)),
	}
	for name, out := range renderings {
		for _, raw := range []string{testUsername, testAPIKey} {
			if strings.Contains(out, raw) {
				t.Errorf("%s leaked credential material %q: %s", name, raw, out)
			}
		}
		if !strings.Contains(out, "****") {
			t.Errorf("%s must render the masked credential form, got %q", name, out)
		}
	}
}

// TestErrorsCarryNoCredentialMaterial pins the error taxonomy hygiene: every
// failure surfaced from the dial plane keeps its message free of credentials
// and credentials out of the structured details.
func TestErrorsCarryNoCredentialMaterial(t *testing.T) {
	rec := conformance.NewRecorder()
	f := newFakeAT(t)
	a := newTestAdapter(t, f, rec.Ingest, nil)
	a.sleep = func(time.Duration) {}
	f.setResponse(func(_ url.Values) (int, http.Header, string) {
		return http.StatusUnauthorized, nil, `{"errorMessage":"Invalid api key"}`
	})
	id := uuid.NewString()
	rec.Seed(testTenant, id, interactions.StatusPending)
	err := a.PlaceCall(context.Background(), telephony.CallCommand{
		InteractionID:   id,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	assertNoCredentialMaterial(t, err, nil)
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newTestAdapter builds a bare adapter against the fake's dial surface (no
// callback server wired — tests that need the callback plane use newHarness).
func newTestAdapter(t *testing.T, f *fakeAT, ingest comms.IngestFunc, mutate func(cfg *Config)) *Adapter {
	t.Helper()
	cfg := Config{
		Username:   testUsername,
		APIKey:     testAPIKey,
		APIBaseURL: f.srv.URL,
		Ingest:     ingest,
		Signer:     testSign,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// expectAppError asserts the exact application-error taxonomy (kind + stable
// machine code) and returns the typed error for further asserts.
func expectAppError(t *testing.T, err error, kind apperrors.Kind, code, what string) *apperrors.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an application error, got nil", what)
		return nil
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("%s: error must be *apperrors.Error, got %T: %v", what, err, err)
		return nil
	}
	if ae.Kind != kind {
		t.Errorf("%s: kind = %q, want %q", what, ae.Kind, kind)
	}
	if ae.Code != code {
		t.Errorf("%s: code = %q, want %q", what, ae.Code, code)
	}
	return ae
}

// awaitEvents polls the recorder until the interaction has at least want
// applied events (async callbacks land on pump goroutines), then returns them.
func awaitEvents(t *testing.T, rec *conformance.Recorder, tenant, id string, want int) []comms.ProviderEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := rec.AppliedFor(tenant, id)
		if len(evs) >= want {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d applied events for %s/%s, got %d (failed deliveries: %d)",
				want, tenant, id, len(evs), len(rec.FailedDeliveries()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// successfulExchanges returns the callback exchanges the adapter's handler
// answered 2xx — pump routing retries (pre-registration 404s) are excluded.
func successfulExchanges(f *fakeAT) []callbackExchange {
	var out []callbackExchange
	for _, x := range f.callbackExchanges() {
		if x.StatusCode >= 200 && x.StatusCode < 300 {
			out = append(out, x)
		}
	}
	return out
}

// awaitStable gives async goroutines a settle window, then asserts the
// applied-event count did not move and that NO delivery failed — the shared
// "the adapter must have done nothing dishonest" canary for unit scenarios.
func awaitStable(t *testing.T, rec *conformance.Recorder, wantApplied int, what string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := len(rec.Applied()); got != wantApplied {
		t.Errorf("%s: applied events = %d, want %d", what, got, wantApplied)
	}
	if got := len(rec.FailedDeliveries()); got != 0 {
		t.Errorf("%s: must not produce failed deliveries, got %d", what, got)
		for _, d := range rec.FailedDeliveries() {
			t.Errorf("  rejected delivery (event=%s interaction=%s tenant=%s): %v", d.Event.Event, d.Event.InteractionID, d.Event.TenantID, d.Err)
		}
	}
}

// assertNoCredentialMaterial scans an error's string form and details for the
// test credentials — the guarantee that provider failures stay reviewable.
func assertNoCredentialMaterial(t *testing.T, err error, ae *apperrors.Error) {
	t.Helper()
	if err == nil {
		return
	}
	out := err.Error()
	var typed *apperrors.Error
	if errors.As(err, &typed) {
		ae = typed
	}
	if ae != nil && ae.Details != nil {
		out += fmt.Sprintf("%v", ae.Details)
	}
	for _, raw := range []string{testUsername, testAPIKey} {
		if strings.Contains(out, raw) {
			t.Errorf("error chain leaked credential material %q: %s", raw, out)
		}
	}
}
