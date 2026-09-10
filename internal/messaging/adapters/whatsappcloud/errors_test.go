package whatsappcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// ─── mapGraphError: the taxonomy table ──────────────────────────────────────

// TestMapGraphErrorTaxonomy walks the full documented mapping table: one row
// per Graph failure class, asserting the typed surface, the application
// surface (Kind + machine code) and the retry verdict. The verdicts are the
// documented semantics: only the provider-signaled transient classes retry.
func TestMapGraphErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		// response under classification
		status int
		code   int
		// expected application surface
		wantKind apperrors.Kind
		wantCode string
		// expected typed surface (one of)
		wantTyped any // *WindowClosedError | *RateLimitedError | *UndeliverableError | *AuthFailedError | *ProviderError
		// expected retry verdict
		wantRetryable bool
	}{
		{
			name:          "131047 window closed",
			status:        http.StatusBadRequest,
			code:          131047,
			wantKind:      apperrors.KindConflict,
			wantCode:      "whatsapp.window_closed",
			wantTyped:     &WindowClosedError{},
			wantRetryable: false,
		},
		{
			name:          "130429 rate limit",
			status:        http.StatusTooManyRequests,
			code:          130429,
			wantKind:      apperrors.KindRateLimited,
			wantCode:      "whatsapp.rate_limited",
			wantTyped:     &RateLimitedError{},
			wantRetryable: true,
		},
		{
			name:          "80007 legacy rate limit",
			status:        http.StatusTooManyRequests,
			code:          80007,
			wantKind:      apperrors.KindRateLimited,
			wantCode:      "whatsapp.rate_limited",
			wantTyped:     &RateLimitedError{},
			wantRetryable: true,
		},
		{
			name:          "131026 undeliverable",
			status:        http.StatusBadRequest,
			code:          131026,
			wantKind:      apperrors.KindInvalid,
			wantCode:      "whatsapp.undeliverable",
			wantTyped:     &UndeliverableError{},
			wantRetryable: false,
		},
		{
			name:          "190 auth failed",
			status:        http.StatusUnauthorized,
			code:          190,
			wantKind:      apperrors.KindUnauth,
			wantCode:      "whatsapp.auth_failed",
			wantTyped:     &AuthFailedError{},
			wantRetryable: false,
		},
		{
			name:          "401 without graph code",
			status:        http.StatusUnauthorized,
			code:          0,
			wantKind:      apperrors.KindUnauth,
			wantCode:      "whatsapp.auth_failed",
			wantTyped:     &AuthFailedError{},
			wantRetryable: false,
		},
		{
			name:          "429 with unrecognized code (status wins)",
			status:        http.StatusTooManyRequests,
			code:          100,
			wantKind:      apperrors.KindRateLimited,
			wantCode:      "whatsapp.rate_limited",
			wantTyped:     &RateLimitedError{},
			wantRetryable: true,
		},
		{
			name:          "5xx transient",
			status:        http.StatusInternalServerError,
			code:          0,
			wantKind:      apperrors.KindInternal,
			wantCode:      "whatsapp.provider_error",
			wantTyped:     &ProviderError{},
			wantRetryable: true,
		},
		{
			name:          "unknown 4xx code (conservative catch-all)",
			status:        http.StatusBadRequest,
			code:          100,
			wantKind:      apperrors.KindInternal,
			wantCode:      "whatsapp.provider_error",
			wantTyped:     &ProviderError{},
			wantRetryable: false,
		},
		{
			name:          "non-graph body 4xx",
			status:        http.StatusForbidden,
			code:          -1, // marker: body is not a Graph envelope
			wantKind:      apperrors.KindInternal,
			wantCode:      "whatsapp.provider_error",
			wantTyped:     &ProviderError{},
			wantRetryable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.code >= 0 {
				body = mustJSON(t, graphErrBody(tc.code, "fixture details"))
			} else {
				body = []byte("<html>proxy error page</html>")
			}
			ga := mapGraphError(tc.status, body, http.Header{})

			if ga.retryable != tc.wantRetryable {
				t.Errorf("retryable = %v, want %v", ga.retryable, tc.wantRetryable)
			}

			// Invariant: every mapped error speaks the application model —
			// errors.As must reach *apperrors.Error with the exact Kind and
			// stable machine code.
			requireAppErr(t, ga.err, tc.wantKind, tc.wantCode)

			// Invariant: dedicated classes carry their typed errors so
			// callers can branch on identity, not string matching.
			switch tc.wantTyped.(type) {
			case *WindowClosedError:
				var got *WindowClosedError
				if !errors.As(ga.err, &got) {
					t.Errorf("expected *WindowClosedError, got %T", ga.err)
				}
			case *RateLimitedError:
				var got *RateLimitedError
				if !errors.As(ga.err, &got) {
					t.Errorf("expected *RateLimitedError, got %T", ga.err)
				}
			case *UndeliverableError:
				var got *UndeliverableError
				if !errors.As(ga.err, &got) {
					t.Errorf("expected *UndeliverableError, got %T", ga.err)
				}
			case *AuthFailedError:
				var got *AuthFailedError
				if !errors.As(ga.err, &got) {
					t.Errorf("expected *AuthFailedError, got %T", ga.err)
				}
			case *ProviderError:
				var got *ProviderError
				if !errors.As(ga.err, &got) {
					t.Errorf("expected *ProviderError, got %T", ga.err)
				}
				if got.Code != tc.code && tc.code > 0 {
					t.Errorf("ProviderError must preserve the Graph code %d, got %d", tc.code, got.Code)
				}
				if got.HTTPStatus != tc.status {
					t.Errorf("ProviderError must carry HTTP status %d, got %d", tc.status, got.HTTPStatus)
				}
			}
		})
	}
}

// ─── the 24-hour window: the documented, tested contract ────────────────────

// TestWindowClosedErrorSemantics pins the honest 24h-window handling:
// typed identity, both error surfaces, the template escape documented in the
// client-safe message, provider diagnostics preserved, and NO retry.
func TestWindowClosedErrorSemantics(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	fake.failNext(1, http.StatusBadRequest, 131047,
		"Message failed to send because more than 24 hours have passed since the customer last replied to this number")

	err := p.Send(context.Background(), validMsg())

	// Typed surface: the explicit, branchable error.
	var wce *WindowClosedError
	if !errors.As(err, &wce) {
		t.Fatalf("free-form send outside the window must surface *WindowClosedError, got %T: %v", err, err)
	}
	if wce.ProviderCode != 131047 {
		t.Errorf("ProviderCode must be 131047, got %d", wce.ProviderCode)
	}
	if !strings.Contains(wce.Detail, "24 hours") {
		t.Errorf("Meta's error_data.details must be preserved for operators, got %q", wce.Detail)
	}

	// Application surface: conflict semantics — the request is well-formed
	// but conflicts with the conversation state (window closed).
	ae := requireAppErr(t, err, apperrors.KindConflict, "whatsapp.window_closed")

	// Invariant: the client-safe message documents the template escape —
	// a caller reading the 409 envelope learns the legitimate next step
	// without reading the source.
	if !strings.Contains(ae.Message, "template") {
		t.Errorf("window error message must document the template escape, got %q", ae.Message)
	}

	// Invariant: NEVER retried. Re-sending the same free-form body cannot
	// succeed — the window is closed until the CUSTOMER replies — so every
	// extra attempt is pure provider load.
	if got := fake.count(); got != 1 {
		t.Errorf("window-closed failures must not be retried, got %d attempts", got)
	}
}

// TestWindowClosedFrom429 pins the code-first classification: some Graph
// deployments attach 131047 to a 429 response; the WINDOW semantics win over
// the status heuristic (re-sending is futile either way), so the typed
// window error must surface, not the retryable rate-limit error.
func TestWindowClosedFrom429(t *testing.T) {
	ga := mapGraphError(http.StatusTooManyRequests,
		mustJSON(t, graphErrBody(131047, "")), http.Header{})
	var wce *WindowClosedError
	if !errors.As(ga.err, &wce) {
		t.Fatalf("131047 must classify as the window error regardless of status, got %T", ga.err)
	}
	if ga.retryable {
		t.Error("window-closed errors must never be retryable")
	}
}

// TestRateLimitedErrorCarriesRetryAfter pins the Retry-After propagation
// into the typed error (callers deciding their own backoff can honor it).
func TestRateLimitedErrorCarriesRetryAfter(t *testing.T) {
	hdr := http.Header{"Retry-After": []string{"7"}}
	ga := mapGraphError(http.StatusTooManyRequests,
		mustJSON(t, graphErrBody(130429, "")), hdr)
	var re *RateLimitedError
	if !errors.As(ga.err, &re) {
		t.Fatalf("expected *RateLimitedError, got %T", ga.err)
	}
	if re.RetryAfter != 7*time.Second {
		t.Errorf("RateLimitedError.RetryAfter must carry the provider hint (7s), got %v", re.RetryAfter)
	}
	if ga.retryAfter != 7*time.Second {
		t.Errorf("the retry verdict must carry the same hint for the backoff loop, got %v", ga.retryAfter)
	}
}

// TestSendNeverRetriesPermanentClasses pins the no-retry verdicts at the
// send level for every permanent classification.
func TestSendNeverRetriesPermanentClasses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   int
	}{
		{"window closed", http.StatusBadRequest, 131047},
		{"undeliverable", http.StatusBadRequest, 131026},
		{"auth failed", http.StatusUnauthorized, 190},
		{"unknown 4xx", http.StatusBadRequest, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGraph(t)
			rec := conformance.NewRecorder()
			p := newTestProvider(t, fake, rec, nil)

			fake.failNext(1, tc.status, tc.code, "")

			_ = p.Send(context.Background(), validMsg())
			if got := fake.count(); got != 1 {
				t.Errorf("%s must surface after exactly 1 attempt, got %d", tc.name, got)
			}
		})
	}
}

// TestSendRetriesRateLimitCodesBeforeSurfacing pins that BOTH rate-limit
// codes go through the bounded retry budget before surfacing.
func TestSendRetriesRateLimitCodesBeforeSurfacing(t *testing.T) {
	fake := newFakeGraph(t)
	rec := conformance.NewRecorder()
	p := newTestProvider(t, fake, rec, nil)

	fake.enqueue(
		fakeReply{status: 429, body: graphErrBody(130429, "")},
		fakeReply{status: 429, body: graphErrBody(80007, "")},
		fakeReply{status: 429, body: graphErrBody(130429, "")},
	)

	err := p.Send(context.Background(), validMsg())
	requireAppErr(t, err, apperrors.KindRateLimited, "whatsapp.rate_limited")
	if got := fake.count(); got != 3 {
		t.Errorf("default budget is 3 attempts, got %d", got)
	}
}

// ─── zero-leakage through error renderings ──────────────────────────────────

// TestErrorRenderingsNeverLeakCredentialMaterial is the adversarial battery:
// every mapped error type is rendered through every common surface and must
// never carry the fixture token. The token enters the adapter only via
// registry.Secret and never enters any error, message or trace.
func TestErrorRenderingsNeverLeakCredentialMaterial(t *testing.T) {
	errs := []error{}
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// One send per classified failure so every error path is exercised with
	// the real token in flight.
	rows := []struct {
		status int
		code   int
	}{{400, 131047}, {429, 130429}, {429, 80007}, {400, 131026}, {401, 190}, {500, 0}, {400, 100}}
	for _, r := range rows {
		f2 := newFakeGraph(t)
		p2 := newTestProvider(t, f2, conformance.NewRecorder(), nil)
		f2.failNext(1, r.status, r.code, "fixture details")
		collect(p2.Send(context.Background(), validMsg()))
	}

	for i, err := range errs {
		renderings := []string{
			err.Error(),
			fmt.Sprintf("%v", err),
			fmt.Sprintf("%s", err),
			fmt.Sprintf("%q", err),
		}
		for j, r := range renderings {
			if strings.Contains(r, testAccessToken) {
				t.Errorf("error #%d rendering #%d leaks credential material: %s", i, j, r)
			}
		}
	}
}

// mustJSON marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
