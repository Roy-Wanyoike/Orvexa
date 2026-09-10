package whatsappcloud

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// TestRetryRecoversFromTransient5xx pins the bounded retry contract: a
// provider-signaled transient failure (5xx) is retried within the budget and
// a later attempt succeeds — one flaky response must not fail a message the
// carrier would have accepted.
func TestRetryRecoversFromTransient5xx(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	sleep := &sleepRecorder{}
	p := newTestProvider(t, fake, rec, sleep.sleep)

	fake.failNext(1, http.StatusInternalServerError, 0, "")

	if err := p.Send(context.Background(), validMsg()); err != nil {
		t.Fatalf("Send must succeed after the transient failure, got: %v", err)
	}
	if got := fake.count(); got != 2 {
		t.Fatalf("expected exactly 2 attempts (1 failed + 1 success), got %d", got)
	}
	// Invariant: backoff is exponential from BaseDelay, jitter-free.
	if got := sleep.recorded(); len(got) != 1 || got[0] != (200*time.Millisecond).String() {
		t.Errorf("recorded backoff must be [200ms], got %v", got)
	}
}

// TestRetryHonorsRetryAfterWithinCap pins the Retry-After contract: the
// provider's hint is honored when it fits under MaxDelay, and capped when it
// does not (a provider asking for minutes of backoff must not stall the
// caller's goroutine for minutes inside an adapter retry loop).
func TestRetryHonorsRetryAfterWithinCap(t *testing.T) {
	cases := []struct {
		name       string
		retryAfter string
		maxDelay   time.Duration
		wantDelay  string
	}{
		{"hint under cap is honored", "1", 10 * time.Second, (1 * time.Second).String()},
		{"hint over cap is capped", "3600", 2 * time.Second, (2 * time.Second).String()},
		{"non-parsable hint is ignored", "soon", 10 * time.Second, (200 * time.Millisecond).String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGraph(t)
			rec := conformance.NewRecorder()
			sleep := &sleepRecorder{}
			p := newTestProvider(t, fake, rec, sleep.sleep)
			p.client.retry.MaxDelay = tc.maxDelay

			fake.enqueue(fakeReply{
				status:  http.StatusTooManyRequests,
				body:    graphErrBody(codeRateLimitHit, ""),
				headers: map[string]string{"Retry-After": tc.retryAfter},
			})

			if err := p.Send(context.Background(), validMsg()); err != nil {
				t.Fatalf("Send must succeed after the rate-limit blip, got: %v", err)
			}
			if got := fake.count(); got != 2 {
				t.Fatalf("expected exactly 2 attempts, got %d", got)
			}
			if got := sleep.recorded(); len(got) != 1 || got[0] != tc.wantDelay {
				t.Errorf("recorded backoff must be [%s], got %v", tc.wantDelay, got)
			}
		})
	}
}

// TestRetryExhaustionSurfacesRateLimited pins the exhaustion semantics for
// persistent rate limiting (Graph 130429 / 80007): every budgeted attempt is
// spent, and the surfaced error is the typed rate-limit error unwrapping to
// KindRateLimited / whatsapp.rate_limited.
func TestRetryExhaustionSurfacesRateLimited(t *testing.T) {
	cases := map[string]int{"130429": codeRateLimitHit, "80007": codeRateLimitLegacy}
	for name, code := range cases {
		t.Run("graph code "+name, func(t *testing.T) {
			fake := newFakeGraph(t)
			rec := conformance.NewRecorder()
			p := newTestProvider(t, fake, rec, nil)

			fake.failNext(5, http.StatusTooManyRequests, code, "")

			err := p.Send(context.Background(), validMsg())
			var re *RateLimitedError
			if !errors.As(err, &re) {
				t.Fatalf("exhausted retries must surface *RateLimitedError, got %T: %v", err, err)
			}
			if re.ProviderCode != code {
				t.Errorf("ProviderCode must be preserved (%d), got %d", code, re.ProviderCode)
			}
			requireAppErr(t, err, apperrors.KindRateLimited, "whatsapp.rate_limited")
			// Invariant: the budget is bounded — exactly MaxAttempts requests.
			if got := fake.count(); got != 3 {
				t.Errorf("retry budget must be 3 attempts (default), got %d", got)
			}
		})
	}
}

// TestRetryExhaustionSurfacesProviderError pins the 5xx exhaustion shape.
func TestRetryExhaustionSurfacesProviderError(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	fake.failNext(5, http.StatusBadGateway, 0, "")

	err := p.Send(context.Background(), validMsg())
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("exhausted 5xx retries must surface *ProviderError, got %T: %v", err, err)
	}
	if pe.HTTPStatus != http.StatusBadGateway {
		t.Errorf("ProviderError must carry the HTTP status, got %d", pe.HTTPStatus)
	}
	if got := fake.count(); got != 3 {
		t.Errorf("retry budget must be 3 attempts, got %d", got)
	}
	requireAppErr(t, err, apperrors.KindInternal, "whatsapp.provider_error")
}

// TestNonGraphErrorBody pins the classification of a 5xx response that does
// not speak the Graph envelope (proxy HTML page, empty body): classified by
// HTTP status, retryable, and the typed error still carries what is known.
func TestNonGraphErrorBody(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	fake.failNext(3, http.StatusServiceUnavailable, 0, "") // exhaust the budget
	err := p.Send(context.Background(), validMsg())
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("must surface *ProviderError, got %T: %v", err, err)
	}
	if pe.HTTPStatus != 503 || pe.Code != 0 {
		t.Errorf("ProviderError must carry status=503 code=0, got status=%d code=%d", pe.HTTPStatus, pe.Code)
	}
	requireAppErr(t, err, apperrors.KindInternal, "whatsapp.provider_error")
}

// TestTransportErrorsAreNotRetried pins the retry-domain split: transport
// failures (connection reset, timeouts) are the CORE's retry domain
// (messaging.Service re-invokes provider.Send after transport timeouts).
// An adapter retrying both layers would multiply the effective budget
// beyond the documented bound.
func TestTransportErrorsAreNotRetried(t *testing.T) {
	// A port with nothing listening: every dial is a deterministic
	// transport error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // free the port; nothing will be listening

	rec := conformance.NewRecorder()
	p, err := New(Config{
		PhoneNumberID: testPhoneNumberID,
		AccessToken:   testAccessToken,
		APIBaseURL:    "http://" + addr,
		Ingest:        rec.Ingest,
		Signer:        testSigner,
		Retry:         RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sendErr := p.Send(context.Background(), validMsg())
	if sendErr == nil {
		t.Fatal("Send against a closed endpoint must fail")
	}
	var ae *apperrors.Error
	if errors.As(sendErr, &ae) && ae.Code == "whatsapp.provider_error" {
		t.Errorf("transport errors must surface raw (not as provider classifications), got: %v", sendErr)
	}
}

// TestContextCancelInterruptsBackoff pins the context contract: a canceled
// context interrupts the retry backoff immediately and the context error —
// not the last provider error — surfaces.
func TestContextCancelInterruptsBackoff(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)
	p.client.retry.Sleep = sleepCtx // real context-aware sleep

	fake.failNext(1, http.StatusTooManyRequests, codeRateLimitHit, "")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := p.Send(ctx, validMsg())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a context canceled during backoff must surface the context error, got: %v", err)
	}
}

// TestLargeResponsesAreBounded pins the bounded-read guard: a runaway
// provider response is truncated, not buffered into memory.
func TestLargeResponsesAreBounded(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	huge := strings.Repeat("x", 5*maxResponseBodyBytes)
	fake.enqueue(fakeReply{status: 200, body: map[string]any{"messages": []any{map[string]any{"id": huge}}}})

	// The oversized id fails the correlation guard loudly (it cannot be a
	// real wamid the statuses will reference); the point of this test is
	// that reading the response terminates instead of buffering unbounded.
	err := p.Send(context.Background(), validMsg())
	if err == nil {
		t.Fatal("an oversized message id must fail the correlation guard")
	}
	requireAppErr(t, err, apperrors.KindInternal, "whatsapp.malformed_send_response")
}

// TestRetryAfterParsingUnit exercises the header parser directly.
func TestRetryAfterParsingUnit(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"1", 1 * time.Second},
		{" 2 ", 2 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"soon", 0},
		{"", 0},
		{"31536000", 365 * 24 * time.Hour}, // one year of seconds parses intact
		// Absurd hints saturate at the max duration instead of overflowing
		// negative; the retry loop caps the honored value anyway.
		{"9223372036854775807", time.Duration(math.MaxInt64)},
	}
	for _, tc := range cases {
		if got := retryAfterSeconds(tc.in); got != tc.want {
			t.Errorf("retryAfterSeconds(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestBackoffExponentialGrowth pins the backoff curve directly.
func TestBackoffExponentialGrowth(t *testing.T) {
	c := &graphClient{retry: RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}.withDefaults()}
	cases := []struct {
		attempt    int
		retryAfter time.Duration
		want       time.Duration
	}{
		{1, 0, 100 * time.Millisecond},
		{2, 0, 200 * time.Millisecond},
		{3, 0, 400 * time.Millisecond},
		{4, 0, 800 * time.Millisecond},
		{5, 0, time.Second},                                 // capped
		{6, 0, time.Second},                                 // still capped (no overflow growth)
		{1, 700 * time.Millisecond, 700 * time.Millisecond}, // hint wins
		{1, 5 * time.Second, time.Second},                   // hint capped
	}
	for _, tc := range cases {
		if got := c.backoff(tc.attempt, tc.retryAfter); got != tc.want {
			t.Errorf("backoff(attempt=%d, after=%v) = %v, want %v", tc.attempt, tc.retryAfter, got, tc.want)
		}
	}
}

// TestResponseBodyReadFailure pins the read-error classification.
func TestResponseBodyReadFailure(t *testing.T) {
	// A client whose transport returns a body that fails mid-read is
	// synthetic here: use a custom RoundTripper.
	p, err := New(Config{
		PhoneNumberID: testPhoneNumberID,
		AccessToken:   testAccessToken,
		Ingest:        func(context.Context, string, []byte, string) error { return nil },
		Signer:        testSigner,
		Retry:         RetryPolicy{Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.client.http = &http.Client{Transport: failingBodyTransport{}}
	err = p.Send(context.Background(), validMsg())
	requireAppErr(t, err, apperrors.KindInternal, "whatsapp.response_read_failed")
}

// failingBodyTransport returns a response whose body errors on read.
type failingBodyTransport struct{}

func (failingBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(errReader{}),
	}, nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
