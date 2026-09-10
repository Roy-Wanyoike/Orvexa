package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error must be *apperrors.Error, got %T: %v", err, err)
	}
	return ae.Code
}

func requireNoPost(t *testing.T, f *fakeTwilio, what string) {
	t.Helper()
	if got := f.postCount(); got != 0 {
		t.Errorf("%s: the carrier must not be touched, got %d API POSTs", what, got)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	f := newFakeTwilio(t)
	for name, cfg := range map[string]Config{
		"missing account sid": {AuthToken: registry.Secret("tok")},
		"missing auth token":  {AccountSID: registry.Secret("ACxxxx")},
		"missing both":        {},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(cfg)
			if codeOf(t, err) != "twilio.credentials_required" {
				t.Errorf("want twilio.credentials_required, got %v", err)
			}
			// Invariant: the failure message names the env vars, never values.
			if !strings.Contains(err.Error(), "ORVEXA_TWILIO_ACCOUNT_SID") {
				t.Errorf("failure must name the missing env var, got: %v", err)
			}
		})
	}
	requireNoPost(t, f, "config validation")
}

func TestNewRejectsIngestWithoutSigner(t *testing.T) {
	f := newFakeTwilio(t)
	_, err := New(Config{
		AccountSID: registry.Secret(testAccountSID),
		AuthToken:  registry.Secret(testAuthToken),
		Ingest:     func(context.Context, string, []byte, string) error { return nil },
	})
	if codeOf(t, err) != "twilio.ingest_not_wired" {
		t.Errorf("want twilio.ingest_not_wired, got %v", err)
	}
	requireNoPost(t, f, "config validation")
}

func TestMaxRetriesClampedToCeiling(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.MaxRetries = 10 })
	if a.maxRetries != maxRetriesCeiling {
		t.Errorf("MaxRetries must clamp to the ≤3 ceiling, got %d", a.maxRetries)
	}
	a = newTestAdapter(t, f, func(c *Config) { c.MaxRetries = -1 })
	if a.maxRetries != 0 {
		t.Errorf("negative MaxRetries must disable retries, got %d", a.maxRetries)
	}
	a = newTestAdapter(t, f, nil)
	if a.maxRetries != defaultMaxRetries {
		t.Errorf("zero MaxRetries must select the default 3, got %d", a.maxRetries)
	}
}

// TestSendFormEncodedPost proves the wire contract: POST form-encoding to the
// versioned account-scoped Messages.json resource, basic auth, From/To/Body
// present, and a StatusCallback URL carrying the interaction + tenant
// routing keys.
func TestSendFormEncodedPost(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	msg := validMsg("tenant-1")

	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.postCount(); got != 1 {
		t.Fatalf("exactly one API POST expected, got %d", got)
	}
	p := f.lastPost()
	wantPath := "/2010-04-01/Accounts/" + testAccountSID + "/Messages.json"
	if p.path != wantPath {
		t.Errorf("path must be %s, got %s", wantPath, p.path)
	}
	if p.authUser != testAccountSID || p.authPass != testAuthToken {
		t.Errorf("basic auth must be AccountSID:AuthToken, got user=%q pass-len=%d", p.authUser, len(p.authPass))
	}
	if got := p.form.Get("From"); got != "+15005550006" {
		t.Errorf("From must be the E.164 sender, got %q", got)
	}
	// Invariant: E.164 normalization reuses the customers helper — spaces
	// collapse, the '+' prefix is canonical.
	if got := p.form.Get("To"); got != "+254711111111" {
		t.Errorf("To must be normalized to +254711111111, got %q", got)
	}
	if got := p.form.Get("Body"); got != msg.Body {
		t.Errorf("Body must travel verbatim, got %q", got)
	}
	cb := p.form.Get("StatusCallback")
	if cb == "" {
		t.Fatal("StatusCallback must be set when StatusCallbackBase is configured")
	}
	if !strings.HasPrefix(cb, f.cbBase+"?") {
		t.Errorf("StatusCallback must extend the configured base, got %q", cb)
	}
	if !strings.Contains(cb, "interaction_id="+msg.InteractionID) {
		t.Errorf("StatusCallback must carry the interaction id, got %q", cb)
	}
	if !strings.Contains(cb, "tenant_id=tenant-1") {
		t.Errorf("StatusCallback must carry the tenant id, got %q", cb)
	}
}

func TestSendFromFallsBackToConfiguredNumber(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	msg := validMsg("tenant-1")
	msg.From = ""

	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.lastPost().form.Get("From"); got != "+15005550006" {
		t.Errorf("From must fall back to the configured number, got %q", got)
	}
}

func TestSendRequiresFrom(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.FromNumber = "" })
	msg := validMsg("tenant-1")
	msg.From = ""

	if err := a.Send(context.Background(), msg); err == nil {
		t.Fatal("Send without any sender must fail")
	} else if codeOf(t, err) != "twilio.from_required" {
		t.Errorf("want twilio.from_required, got %v", err)
	}
	requireNoPost(t, f, "missing sender")
}

func TestSendE164NormalizationErrors(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	for name, to := range map[string]string{
		"letters":     "not-a-phone",
		"too short":   "+123",
		"punctuation": "+254(711)111-111",
	} {
		t.Run(name, func(t *testing.T) {
			msg := validMsg("tenant-1")
			msg.To = to
			err := a.Send(context.Background(), msg)
			if err == nil {
				t.Fatal("invalid destination must fail")
			}
			var ae *apperrors.Error
			if !errors.As(err, &ae) || ae.Code != "twilio.invalid_phone" {
				t.Errorf("want twilio.invalid_phone, got %v", err)
			}
			requireNoPost(t, f, "invalid destination")
		})
	}
}

func TestSendRejectsWithoutTenantContext(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	msg := validMsg("")

	err := a.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send without tenant context must be rejected")
	}
	if codeOf(t, err) != "comms.tenant_required" {
		t.Errorf("want comms.tenant_required, got %v", err)
	}
	requireNoPost(t, f, "tenant-less send")
}

func TestSendMediaURLsPassThrough(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	msg := validMsg("tenant-1")
	msg.Body = ""
	msg.MediaURLs = []string{
		"https://cdn.example.test/media-a.jpg",
		"https://cdn.example.test/media-b.jpg",
	}

	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	form := f.lastPost().form
	if got := form["MediaUrl"]; len(got) != 2 || got[0] != msg.MediaURLs[0] || got[1] != msg.MediaURLs[1] {
		t.Errorf("media URLs must pass through in order, got %v", got)
	}
}

func TestSendRejectsNonHTTPMediaScheme(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	msg := validMsg("tenant-1")
	msg.MediaURLs = []string{"ftp://cdn.example.test/media-a.jpg"}

	if err := a.Send(context.Background(), msg); err == nil {
		t.Fatal("non-http(s) media must be rejected")
	} else if codeOf(t, err) != "twilio.invalid_media" {
		t.Errorf("want twilio.invalid_media, got %v", err)
	}
	requireNoPost(t, f, "invalid media scheme")
}

// TestSendDuplicateInteractionIDSilentNoOp proves Simulator semantics: the
// same InteractionID never errors and never reaches the carrier twice — a
// second real POST would double-bill the customer.
func TestSendDuplicateInteractionIDSilentNoOp(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	msg := validMsg("tenant-1")

	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	for i := 2; i <= 4; i++ {
		if err := a.Send(context.Background(), msg); err != nil {
			t.Fatalf("duplicate Send #%d must be a silent no-op, got: %v", i, err)
		}
	}
	if got := f.postCount(); got != 1 {
		t.Errorf("duplicates must not re-POST (double-billing), got %d POSTs", got)
	}
}

// TestSendFailureReleasesClaim proves a fully-failed send frees the
// InteractionID so a later call is a genuine retry, not a suppressed dup.
func TestSendFailureReleasesClaim(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.MaxRetries = -1 }) // fail fast
	msg := validMsg("tenant-1")

	f.scriptResponses(fakeStatus{status: 500}, fakeStatus{status: 500})
	if err := a.Send(context.Background(), msg); err == nil {
		t.Fatal("send against scripted 500s must fail")
	}
	f.resetScript()
	if err := a.Send(context.Background(), msg); err != nil {
		t.Fatalf("retry after full failure must re-POST (claim released): %v", err)
	}
	// 1 POST consumed the failed send (MaxRetries=-1 → terminal 500, the
	// second scripted 500 unused), 1 POST for the released-claim retry.
	if got := f.postCount(); got != 2 {
		t.Errorf("expected 1 failed POST + 1 retry POST, got %d", got)
	}
}

// TestSendRetriesBounded proves the ≤3 bounded retry on 429/5xx: successes
// recover mid-script, exhaustion fails with a stable code, and every sleep
// stays inside the jitter ceiling.
func TestSendRetriesBounded(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil) // MaxRetries=3, backoff 1ms..4ms
	rec := sleepSeam(a)

	t.Run("recovers within the bound", func(t *testing.T) {
		f.resetScript()
		f.scriptResponses(
			fakeStatus{status: 429},
			fakeStatus{status: 500},
			fakeStatus{status: 429},
		)
		if err := a.Send(context.Background(), validMsg("tenant-1")); err != nil {
			t.Fatalf("429/5xx script must recover within 3 retries, got: %v", err)
		}
		if got := f.postCount(); got != 4 {
			t.Errorf("1 initial + 3 retries expected, got %d", got)
		}
		for i, d := range rec.all() {
			if d < 0 || d > 4*time.Millisecond {
				t.Errorf("sleep #%d = %s must be full-jittered within the 4ms ceiling", i, d)
			}
		}
	})

	t.Run("429 exhaustion fails with a stable code", func(t *testing.T) {
		f.resetScript()
		f.scriptResponses(
			fakeStatus{status: 429}, fakeStatus{status: 429},
			fakeStatus{status: 429}, fakeStatus{status: 429}, fakeStatus{status: 429},
		)
		err := a.Send(context.Background(), validMsg("tenant-1"))
		if err == nil {
			t.Fatal("persistent 429 must exhaust the bounded retries")
		}
		if codeOf(t, err) != "twilio.rate_limited" {
			t.Errorf("want twilio.rate_limited, got %v", err)
		}
		if got := f.postCount(); got != 8 { // 4 (recovery) + 4 (this scenario)
			t.Errorf("1 initial + 3 retries expected in this scenario, got %d total", got-4)
		}
	})

	t.Run("5xx exhaustion fails with a stable code", func(t *testing.T) {
		f.resetScript()
		f.scriptResponses(
			fakeStatus{status: 503}, fakeStatus{status: 503},
			fakeStatus{status: 503}, fakeStatus{status: 503},
		)
		err := a.Send(context.Background(), validMsg("tenant-1"))
		if err == nil {
			t.Fatal("persistent 5xx must exhaust the bounded retries")
		}
		if codeOf(t, err) != "twilio.upstream_error" {
			t.Errorf("want twilio.upstream_error, got %v", err)
		}
	})
}

// TestSendRetryAfterHonored proves explicit Retry-After guidance is honored
// exactly (clamped to the ceiling), not jittered away.
func TestSendRetryAfterHonored(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.RetryMaxBackoff = 5 * time.Millisecond })
	rec := sleepSeam(a)

	f.scriptResponses(fakeStatus{status: 429, retryAfter: "30"}, fakeStatus{status: 500})
	if err := a.Send(context.Background(), validMsg("tenant-1")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := rec.all()
	if len(got) != 2 {
		t.Fatalf("two retries expected, got %d sleeps", len(got))
	}
	if got[0] != 5*time.Millisecond {
		t.Errorf("Retry-After=30s must clamp to the 5ms ceiling, got %s", got[0])
	}
}

// TestSendTerminalErrorsNoRetry proves 4xx is terminal: one attempt, no
// sleeps, taxonomy-mapped error.
func TestSendTerminalErrorsNoRetry(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	rec := sleepSeam(a)

	cases := []struct {
		status   int
		wantKind apperrors.Kind
		wantCode string
	}{
		{http.StatusBadRequest, apperrors.KindInvalid, "twilio.request_rejected"},
		{http.StatusUnauthorized, apperrors.KindUnauth, "twilio.auth_failed"},
		{http.StatusForbidden, apperrors.KindForbidden, "twilio.forbidden"},
		{http.StatusNotFound, apperrors.KindNotFound, "twilio.not_found"},
		{http.StatusGone, apperrors.KindNotFound, "twilio.not_found"},
		{http.StatusUnprocessableEntity, apperrors.KindInvalid, "twilio.request_rejected"},
		{http.StatusConflict, apperrors.KindInvalid, "twilio.request_rejected"},
	}
	postsBefore := 0
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			f.resetScript()
			f.scriptResponses(fakeStatus{status: tc.status})
			err := a.Send(context.Background(), validMsg("tenant-1"))
			var ae *apperrors.Error
			if !errors.As(err, &ae) {
				t.Fatalf("want *apperrors.Error, got %v", err)
			}
			if ae.Kind != tc.wantKind || ae.Code != tc.wantCode {
				t.Errorf("want %s/%s, got %s/%s", tc.wantKind, tc.wantCode, ae.Kind, ae.Code)
			}
			postsBefore++
			if got := f.postCount(); got != postsBefore {
				t.Errorf("terminal errors must not retry: expected %d total posts, got %d", postsBefore, got)
			}
		})
	}
	if got := len(rec.all()); got != 0 {
		t.Errorf("terminal errors must never sleep, got %d sleeps", got)
	}
}

// TestSendProviderErrorDetailsBounded proves provider error text reaches
// details only, bounded, with no credential material.
func TestSendProviderErrorDetailsBounded(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.MaxRetries = -1 })
	f.scriptResponses(fakeStatus{status: 400})

	err := a.Send(context.Background(), validMsg("tenant-1"))
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *apperrors.Error, got %v", err)
	}
	d, ok := ae.Details.(map[string]any)
	if !ok {
		t.Fatalf("error must carry structured details, got %T", ae.Details)
	}
	if d["twilio_code"] != 21211 {
		t.Errorf("twilio_code detail expected, got %v", d["twilio_code"])
	}
	if !strings.Contains(fmt.Sprint(d["twilio_message"]), "not a valid phone number") {
		t.Errorf("provider message detail expected, got %v", d["twilio_message"])
	}
	// Invariant: neither the error chain nor the details carry credentials
	// or the account SID.
	rendered := fmt.Sprintf("%v | %+v", err, d)
	if strings.Contains(rendered, testAccountSID) || strings.Contains(rendered, testAuthToken) {
		t.Errorf("credential material leaked into error rendering: %s", rendered)
	}
}

// TestSendNoStatusCallbackWhenUnconfigured proves the documented
// fire-and-forget posture: no callback base → no StatusCallback parameter.
func TestSendNoStatusCallbackWhenUnconfigured(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.StatusCallbackBase = "" })
	if err := a.Send(context.Background(), validMsg("tenant-1")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := f.lastPost().form.Get("StatusCallback"); got != "" {
		t.Errorf("StatusCallback must be absent, got %q", got)
	}
}

func TestSendInvalidStatusCallbackBase(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.StatusCallbackBase = "not a url" })
	err := a.Send(context.Background(), validMsg("tenant-1"))
	if err == nil {
		t.Fatal("malformed StatusCallbackBase must fail the send")
	}
	if codeOf(t, err) != "twilio.invalid_status_callback_base" {
		t.Errorf("want twilio.invalid_status_callback_base, got %v", err)
	}
}

// TestSendContextCanceled proves ctx cancellation surfaces before any dial.
func TestSendContextCanceled(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Send(ctx, validMsg("tenant-1")); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled ctx must surface as context.Canceled, got %v", err)
	}
	requireNoPost(t, f, "canceled context")
}

// TestTransportErrorRedactsAccountSID proves network failures never carry
// the account SID (the API URL embeds it) into the error chain.
func TestTransportErrorRedactsAccountSID(t *testing.T) {
	f := newFakeTwilio(t)
	a := newTestAdapter(t, f, func(c *Config) { c.StatusCallbackBase = "" })
	f.apiSrv.Close() // force a dial failure

	err := a.Send(context.Background(), validMsg("tenant-1"))
	if err == nil {
		t.Fatal("transport failure must surface")
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) || ae.Code != "twilio.transport_error" {
		t.Fatalf("want twilio.transport_error, got %v", err)
	}
	rendered := fmt.Sprintf("%v", err)
	if cause := errors.Unwrap(err); cause != nil {
		rendered += " | " + fmt.Sprintf("%v | %#v", cause, cause)
	}
	if strings.Contains(rendered, testAccountSID) || strings.Contains(rendered, testAuthToken) {
		t.Errorf("account SID leaked through transport error: %s", rendered)
	}
	if !strings.Contains(rendered, "[redacted]") {
		t.Errorf("transport error must carry the redacted endpoint, got: %s", rendered)
	}
}

// TestCredentialRedactionOnEveryRenderingPath proves the registry.Secret
// contract on adapter credentials: no fmt verb, slog or JSON rendering leaks
// the raw values; only the sanctioned ****last4 survives.
func TestCredentialRedactionOnEveryRenderingPath(t *testing.T) {
	f := newFakeTwilio(t)
	long := "live-secret-auth-token-9999" // >12 bytes → last-4 reveal allowed
	a := newTestAdapter(t, f, func(c *Config) {
		c.AuthToken = registry.Secret(long)
	})
	cfg := Config{
		AccountSID: registry.Secret(testAccountSID),
		AuthToken:  registry.Secret(long),
	}

	renderings := map[string]string{
		"%v":           fmt.Sprintf("%v", cfg),
		"%+v":          fmt.Sprintf("%+v", cfg),
		"%#v":          fmt.Sprintf("%#v", cfg),
		"slog":         slogRender(t, cfg),
		"adapter%#v":   fmt.Sprintf("%#v", a),
		"adapter%+v":   fmt.Sprintf("%+v", a),
		"cfg.String()": cfg.String(),
		"slog cfg":     slogConfigRender(t, cfg),
	}
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	renderings["json"] = string(jsonBytes)

	for name, rendered := range renderings {
		if strings.Contains(rendered, long) || strings.Contains(rendered, testAuthToken) || strings.Contains(rendered, testAccountSID) {
			t.Errorf("%s: credential material leaked: %s", name, rendered)
		}
	}
	if !strings.Contains(renderings["%v"], "****9999") {
		t.Errorf("sanctioned last-4 reveal expected, got: %s", renderings["%v"])
	}
}

func slogRender(t *testing.T, cfg Config) string {
	t.Helper()
	buf := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	logger.Info("config", "sid", cfg.AccountSID, "token", cfg.AuthToken, "from", cfg.FromNumber)
	return buf.String()
}

// slogConfigRender exercises the whole-config structured logging path: with
// Config implementing LogValuer-eligible Stringer semantics, a cfg passed as
// a single log attribute must render redacted, never a raw field dump.
func slogConfigRender(t *testing.T, cfg Config) string {
	t.Helper()
	buf := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(buf, nil))
	logger.Info("twilio config", "config", cfg)
	return buf.String()
}
