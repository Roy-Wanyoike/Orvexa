package africastalking

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms"
	"github.com/Roy-Wanyoike/orvexa/internal/telephony"
)

// Test credentials. Short values on purpose: the redaction tests assert the
// FULL raw forms never render, and sub-12-byte secrets mask completely
// (registry.Redacted reveals a 4-byte suffix only at 12+ bytes).
const (
	testUsername = "at-user-01" // 10 bytes → renders "****"
	testAPIKey   = "atk-9f3c2e" // 11 bytes → renders "****"
	testTenant   = "tenant-at"
)

// testSign is the deterministic HMAC stand-in (the conformance kit's shape):
// the gateway is fail-closed on signatures, so deliveries must be signed,
// but HMAC verification itself is the gateway's concern, not a test's.
func testSign(body []byte) string {
	sum := sha256.Sum256(append([]byte("orvexa-at-voice-test-key\x00"), body...))
	return "sha256=" + hex.EncodeToString(sum[:16])
}

// capturedDial is one make-call request observed by the fake: exactly what
// the adapter put on the wire (credential header, form fields).
type capturedDial struct {
	Method    string
	Path      string
	APIKeyHdr string
	AuthHdr   string // Authorization header (must stay EMPTY — see README)
	Form      url.Values
}

// callbackExchange is one status-callback delivery the fake made to the
// adapter's callback surface, with the adapter's response: the wire proof of
// both directions of AT's callback protocol (status up, action XML down).
type callbackExchange struct {
	Body       []byte
	StatusCode int
	Response   []byte
}

// fakeAT is an httptest fake of the Africa's Talking Voice surface. It
// implements the two wire contracts the adapter speaks:
//
//   - Dial plane (POST /call): form-encoded, authenticated by the apikey
//     header (the documented AT scheme — NOT Bearer, see README.md). The
//     default response is the AT envelope with entry status "Queued" and a
//     minted sessionId; tests can script any status/body via setResponse.
//   - Callback plane: after a queued dial, the fake plays the carrier's side
//     and POSTs the scripted status callbacks (default Queued → InProgress)
//     to the adapter's callback surface over real HTTP, retrying bounded on
//     non-2xx exactly like a carrier retrying a not-yet-routable webhook.
//     Every exchange (sent body + adapter's response) is captured.
type fakeAT struct {
	t *testing.T

	srv    *httptest.Server // dial surface (what Config.APIBaseURL points at)
	cbSrv  *httptest.Server // callback surface (the adapter's StatusCallbackHandler)
	apiKey string
	user   string

	mu          sync.Mutex
	dials       []capturedDial
	exchanges   []callbackExchange
	respond     func(form url.Values) (int, http.Header, string)         // scripted dial response (nil = default)
	callbacks   func(sessionID, clientRequestID string) []StatusCallback // scripted callback stream (nil = Queued, InProgress)
	cbURL       string                                                   // callback URL the pump POSTs to (empty = no pump)
	lastSession string                                                   // last minted sessionId
	lastClient  string                                                   // last echoed clientRequestId
	sessionSeq  int
}

// newFakeAT boots the dial surface only. Wire callbacks with
// serveCallbacks(adapter) once the adapter exists.
func newFakeAT(t *testing.T) *fakeAT {
	t.Helper()
	f := &fakeAT{t: t, apiKey: testAPIKey, user: testUsername}
	mux := http.NewServeMux()
	mux.HandleFunc("/call", f.handleCall)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// Close shuts both surfaces down and waits for in-flight requests.
func (f *fakeAT) Close() {
	f.mu.Lock()
	srv, cb := f.srv, f.cbSrv
	f.mu.Unlock()
	if cb != nil {
		cb.Close()
	}
	if srv != nil {
		srv.Close()
	}
}

// serveCallbacks mounts the adapter's StatusCallbackHandler as a second
// httptest server — the platform's real callback endpoint — and points the
// fake's callback pump at it. This is the e2e path: AT → gateway → adapter.
func (f *fakeAT) serveCallbacks(adapter *Adapter) {
	f.t.Helper()
	cb := httptest.NewServer(adapter.StatusCallbackHandler())
	f.mu.Lock()
	f.cbSrv = cb
	f.cbURL = cb.URL
	f.mu.Unlock()
}

// setResponse scripts dial responses. When the script runs out (or returns
// exhausted) the default queued envelope resumes.
func (f *fakeAT) setResponse(script func(form url.Values) (int, http.Header, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = script
}

// setCallbacks scripts the status callback stream played after each queued
// dial. Nil = the default carrier play (Queued, then InProgress).
func (f *fakeAT) setCallbacks(script func(sessionID, clientRequestID string) []StatusCallback) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callbacks = script
}

// dialRequests returns the captured make-call requests.
func (f *fakeAT) dialRequests() []capturedDial {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedDial, len(f.dials))
	copy(out, f.dials)
	return out
}

// callbackExchanges returns the captured status-callback deliveries and the
// adapter's responses to them.
func (f *fakeAT) callbackExchanges() []callbackExchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]callbackExchange, len(f.exchanges))
	copy(out, f.exchanges)
	return out
}

// lastLeg returns the sessionId/clientRequestId the fake minted and echoed
// for the most recent queued dial.
func (f *fakeAT) lastLeg() (sessionID, clientRequestID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSession, f.lastClient
}

// deliverCallback synchronously plays ONE status callback to the adapter's
// callback surface and records the exchange. This is the test's direct line
// to the carrier's async side — used to prove in-call control actions ride
// the next callback response.
func (f *fakeAT) deliverCallback(cb StatusCallback) callbackExchange {
	f.t.Helper()
	target := f.cbURLNow()
	if target == "" {
		f.t.Fatal("fakeAT: no callback target — call serveCallbacks first")
	}
	status, resp := f.postCallback(target, cb)
	return callbackExchange{Body: mustJSON(f.t, cb), StatusCode: status, Response: resp}
}

// handleCall implements the /call dial contract.
func (f *fakeAT) handleCall(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"errorMessage":"Malformed form"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.dials = append(f.dials, capturedDial{
		Method:    r.Method,
		Path:      r.URL.Path,
		APIKeyHdr: r.Header.Get("apikey"),
		AuthHdr:   r.Header.Get("Authorization"),
		Form:      r.PostForm,
	})
	respond, callbacks := f.respond, f.callbacks
	f.mu.Unlock()

	// Auth check: AT's documented scheme is the apikey header. A wrong or
	// missing key is a 401 with the envelope error shape — never a queued dial.
	if r.Header.Get("apikey") != f.apiKey {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorMessage":"Invalid api key"}`))
		return
	}
	if got := r.PostForm.Get("username"); got != f.user {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errorMessage":"Invalid username"}`))
		return
	}

	status, hdr, body := f.defaultQueued(r.PostForm)
	if respond != nil {
		status, hdr, body = respond(r.PostForm)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))

	// Carrier side of the async contract: only a 2xx queued dial progresses;
	// the pump plays the scripted callbacks over the real callback surface.
	if status >= 200 && status < 300 && f.cbURLNow() != "" {
		var env callResponse
		if json.Unmarshal([]byte(body), &env) == nil && len(env.Entries) > 0 {
			entry := env.Entries[0]
			go f.playCallbacks(callbacks, entry.SessionID, entry.ClientRequestID)
		}
	}
}

// defaultQueued builds AT's success envelope: errorMessage "None", one entry
// per dialed number with status Queued and a freshly minted sessionId.
func (f *fakeAT) defaultQueued(form url.Values) (int, http.Header, string) {
	to := form.Get("to")
	crid := form.Get("clientRequestId")
	f.mu.Lock()
	f.sessionSeq++
	session := fmt.Sprintf("ATVerb_%d_%s", f.sessionSeq, randomHex(6))
	f.lastSession, f.lastClient = session, crid
	f.mu.Unlock()
	env := callResponse{
		ErrorMessage: atErrorMessageNone,
		Entries: []callEntry{{
			PhoneNumber:     to,
			SessionID:       session,
			Status:          StatusQueued,
			ErrorMessage:    atErrorMessageNone,
			ClientRequestID: crid,
		}},
	}
	b, err := json.Marshal(env)
	if err != nil {
		return http.StatusInternalServerError, nil, `{"errorMessage":"fake failure"}`
	}
	return http.StatusOK, nil, string(b)
}

// playCallbacks delivers the scripted (or default) callback stream for one
// dial, in order, on one goroutine — serial delivery keeps ringing before
// connected, exactly like a carrier's status pipeline. Non-2xx responses are
// retried with a small bounded budget (AT retries unroutable webhooks), so a
// callback racing the adapter's leg registration still lands.
func (f *fakeAT) playCallbacks(script func(sessionID, clientRequestID string) []StatusCallback, sessionID, clientRequestID string) {
	cbs := []StatusCallback{
		{Status: StatusQueued},
		{Status: StatusInProgress},
	}
	if script != nil {
		cbs = script(sessionID, clientRequestID)
	}
	target := f.cbURLNow()
	if target == "" || len(cbs) == 0 {
		return
	}
	for _, cb := range cbs {
		if cb.SessionID == "" {
			cb.SessionID = sessionID
		}
		if cb.ClientRequestID == "" {
			cb.ClientRequestID = clientRequestID
		}
		for attempt := 0; attempt < 40; attempt++ {
			status, _ := f.postCallback(target, cb)
			if status >= 200 && status < 300 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// postCallback POSTs one status callback JSON to target and returns the
// response status and body. Never uses t — it runs on pump goroutines that
// may outlive the current test step.
func (f *fakeAT) postCallback(target string, cb StatusCallback) (int, []byte) {
	body, err := json.Marshal(cb)
	if err != nil {
		return 0, nil
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	f.mu.Lock()
	f.exchanges = append(f.exchanges, callbackExchange{Body: body, StatusCode: resp.StatusCode, Response: respBody})
	f.mu.Unlock()
	return resp.StatusCode, respBody
}

// cbURLNow snapshots the callback target under the mutex (pump goroutines
// and the dial handler share it with test setup).
func (f *fakeAT) cbURLNow() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cbURL
}

// harness assembles the full e2e rig for one test: fake carrier + adapter
// (wired to the given ingest + signer) + the adapter's real callback handler
// mounted as the fake's delivery target. The adapter is the system under
// test; the fake is the carrier on both planes.
type harness struct {
	fake    *fakeAT
	adapter *Adapter
}

func newHarness(t *testing.T, ingest comms.IngestFunc, mutate func(cfg *Config)) *harness {
	t.Helper()
	f := newFakeAT(t)
	cfg := Config{
		Username:     testUsername,
		APIKey:       testAPIKey,
		APIBaseURL:   f.srv.URL,
		Ingest:       ingest,
		Signer:       testSign,
		MaxRetries:   maxRetriesCeiling,
		RetryBackoff: time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	ad, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.serveCallbacks(ad)
	return &harness{fake: f, adapter: ad}
}

// placeCall issues the harness's canonical valid dial.
func (h *harness) placeCall(ctx context.Context, interactionID string) error {
	return h.adapter.PlaceCall(ctx, telephony.CallCommand{
		InteractionID:   interactionID,
		From:            "+254700000001",
		To:              "+254711111111",
		ProviderOptions: map[string]any{"tenant_id": testTenant},
	})
}

// mustJSON marshals v or fails the test (test-goroutine only).
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// randomHex returns n random bytes hex-encoded (session id minting).
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}
