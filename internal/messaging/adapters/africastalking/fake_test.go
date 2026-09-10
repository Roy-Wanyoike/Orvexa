// Package africastalking_test exercises the Africa's Talking adapter
// against an httptest fake carrier. The fake pins the AT wire contract the
// adapter speaks: JSON bodies on the version-1 messaging path, the API key
// in the "apiKey" header only, per-recipient synchronous status, and
// out-of-band delivery reports. Lifecycle receipts are observed through the
// conformance kit's Recorder — the same guarded ProviderEvent path the
// platform's webhook gateway feeds.
package africastalking_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/interactions"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging/adapters/africastalking"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

const (
	testTenant = "tenant-at"
	// testAPIKey is an obviously-fake key (>= 12 bytes so the redaction
	// reveal threshold applies). The leak tests assert its unique substring
	// appears ONLY in the apiKey request header — never in bodies, errors,
	// logs, event payloads or Go/JSON renderings.
	testAPIKey = "AKTEST-7f3c9d2e1b4a0f6b"
	testSender = "ORVEXA"
	// leakMarker is the unique substring the zero-leakage test hunts for.
	leakMarker = "7f3c9d2e1b4a0f6b"
	testMSISDN = "+254711223344"
)

// signBody is a real HMAC-SHA256 signer (the kit asserts signature presence,
// never HMAC correctness — that belongs to the gateway's verifier tests).
func signBody(secret string) comms.Signer {
	return func(body []byte) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return hex.EncodeToString(mac.Sum(nil))
	}
}

// recordedSend is one /messaging request the fake observed.
type recordedSend struct {
	Method     string
	Path       string
	APIKey     string
	Username   string
	To         string
	Message    string
	From       string
	RawBody    string
	Headers    http.Header
	statusCode int // what the fake answered with
}

// fakeAT is the fake Africa's Talking server: a /messaging endpoint with a
// scriptable status sequence and a per-request recorder.
type fakeAT struct {
	t      *testing.T
	mu     sync.Mutex
	server *httptest.Server
	sends  []recordedSend

	// script, when set, maps the zero-based call index to (HTTP status,
	// response body). When nil the fake answers 200 with the configured
	// per-recipient status.
	script func(call int) (int, string)
	// recipStatus/recipStatusText fill the default 200 response.
	recipStatus     int
	recipStatusText string
	// retryAfterHeader, when set, is answered as a Retry-After header.
	retryAfterHeader string
}

func newFakeAT(t *testing.T) *fakeAT {
	t.Helper()
	f := &fakeAT{t: t, recipStatus: 102, recipStatusText: "Delivered"}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// URL is the fake's base URL for Config.BaseURL.
func (f *fakeAT) URL() string { return f.server.URL }

// setScript replaces the response script (call index → HTTP status/body).
func (f *fakeAT) setScript(fn func(call int) (int, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = fn
}

func (f *fakeAT) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/messaging" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rec := recordedSend{
		Method: r.Method, Path: r.URL.Path, APIKey: r.Header.Get("apiKey"), Headers: r.Header.Clone(),
	}
	raw, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}
	rec.RawBody = string(raw)
	var req struct {
		Username string `json:"username"`
		To       string `json:"to"`
		Message  string `json:"message"`
		From     string `json:"from"`
	}
	if err := json.Unmarshal(raw, &req); err == nil {
		rec.Username, rec.To, rec.Message, rec.From = req.Username, req.To, req.Message, req.From
	}

	status, payload := f.defaultResponse(len(f.sends))
	if f.script != nil {
		status, payload = f.script(len(f.sends))
	}
	rec.statusCode = status
	f.sends = append(f.sends, rec)

	if f.retryAfterHeader != "" {
		w.Header().Set("Retry-After", f.retryAfterHeader)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(payload))
}

// defaultResponse builds the standard 200 envelope for call i with a unique
// carrier message id per request.
func (f *fakeAT) defaultResponse(call int) (int, string) {
	resp := map[string]any{
		"SMSMessageData": map[string]any{
			"Recipients": []map[string]any{{
				"statusCode": f.recipStatus,
				"status":     f.recipStatusText,
				"messageId":  fmt.Sprintf("ATMsgId_%s_%03d", f.recipStatusText, call),
			}},
		},
	}
	raw, _ := json.Marshal(resp)
	return http.StatusOK, string(raw)
}

// messageID mirrors the carrier message id defaultResponse assigns to the
// zero-based call (tests use it to key delivery reports).
func (f *fakeAT) messageID(call int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("ATMsgId_%s_%03d", f.recipStatusText, call)
}

// recorded returns a copy of the observed sends.
func (f *fakeAT) recorded() []recordedSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedSend(nil), f.sends...)
}

// carrierResponse renders an explicit AT send response (for scripts).
func carrierResponse(statusCode int, status, messageID string) string {
	raw, _ := json.Marshal(map[string]any{
		"SMSMessageData": map[string]any{
			"Recipients": []map[string]any{{
				"statusCode": statusCode,
				"status":     status,
				"messageId":  messageID,
			}},
		},
	})
	return string(raw)
}

// buildAdapter wires the adapter to the fake and the recorder, with the
// fake-test credential set (leak-marker API key) and a real HMAC signer.
func buildAdapter(t *testing.T, f *fakeAT, rec *conformance.Recorder, mutate func(*africastalking.Config)) *africastalking.Adapter {
	t.Helper()
	cfg := africastalking.Config{
		Username: registry.Secret("orvexa-sandbox"),
		APIKey:   registry.Secret(testAPIKey),
		SenderID: registry.Secret(testSender),
		BaseURL:  f.URL(),
		Ingest:   rec.Ingest,
		Signer:   signBody("test-only-signing-secret"),
		TenantID: testTenant,
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

// seedInteraction pre-creates the platform interaction an outbound message
// keys on (messaging.Service creates the interaction and flips it active
// before provider handoff), so the Recorder's guarded processor accepts the
// adapter's receipts instead of failing them as not-found.
func seedInteraction(rec *conformance.Recorder, msg messaging.Message) {
	rec.Seed(msg.TenantID, msg.InteractionID, interactions.StatusActive)
}

// requireCode asserts the error is an application error carrying exactly the
// wanted stable machine code.
func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected application error %q, got nil", want)
	}
	var ae *apperrors.Error
	if !errors.As(err, &ae) {
		t.Fatalf("error must be *apperrors.Error, got %T: %v", err, err)
	}
	if ae.Code != want {
		t.Fatalf("error code must be exactly %q, got %q", want, ae.Code)
	}
}

// capture is a raw ingest hook for the USSD tests: it records every delivery
// (provider label, body, signature) without running the processor — the
// ussd.session.* vocabulary is intentionally not yet applied by
// comms.Processor (documented integration gap in the README), so the USSD
// tests assert at the delivery layer.
type capture struct {
	mu     sync.Mutex
	events []comms.ProviderEvent
	raw    []string
	sigs   []string
	labels []string
	fail   bool // when set, every ingest fails (event-plumbing outage)
}

// hook returns the comms.IngestFunc for the adapter.
func (c *capture) hook(ctx context.Context, provider string, body []byte, signature string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("event plumbing outage")
	}
	var ev comms.ProviderEvent
	_ = json.Unmarshal(body, &ev)
	c.events = append(c.events, ev)
	c.raw = append(c.raw, string(body))
	c.sigs = append(c.sigs, signature)
	c.labels = append(c.labels, provider)
	return nil
}

// snapshot returns copies of the observed deliveries.
func (c *capture) snapshot() (events []comms.ProviderEvent, raw, sigs, labels []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]comms.ProviderEvent(nil), c.events...), append([]string(nil), c.raw...),
		append([]string(nil), c.sigs...), append([]string(nil), c.labels...)
}
