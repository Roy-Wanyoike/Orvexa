package whatsappcloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/conformance"
	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
)

// ─── shared test fixtures ───────────────────────────────────────────────────
//
// Credential values here are deliberately fake, syntactically-plausible
// fixtures (the registry's redaction tests use the same discipline). They
// exist to be asserted ABSENT from every rendering path — never to
// authenticate anything.

const (
	testPhoneNumberID = "109876543210987"
	testAccessToken   = "EAAG-test-fixture-token-0123456789abcdef"
)

// testSigner is a deterministic HMAC stand-in over a fixed, obviously-fake
// key (mirrors the conformance kit's own signer). The kit requires
// signatures to be present and stable; HMAC verification belongs to the
// fail-closed webhook gateway.
func testSigner(body []byte) string {
	sum := sha256.Sum256(append([]byte("orvexa-whatsappcloud-test-key\x00"), body...))
	return "sha256=" + hex.EncodeToString(sum[:16])
}

// noSleep records backoff waits instead of taking them — deterministic and
// fast; the recorded delays are asserted by the retry tests.
type sleepRecorder struct {
	mu     sync.Mutex
	delays []string
}

func (s *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delays = append(s.delays, d.String())
	return nil
}

func (s *sleepRecorder) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delays...)
}

// ─── fake Graph server ──────────────────────────────────────────────────────

// capturedRequest is one request the fake Graph server observed.
type capturedRequest struct {
	Method      string
	Path        string
	Auth        string
	ContentType string
	RawBody     []byte
	Body        map[string]any
}

// fakeReply is one scripted response (popped per request before the default
// success path applies).
type fakeReply struct {
	status  int
	body    any
	headers map[string]string
}

// graphErrBody renders a Graph error envelope as the fake would send it.
func graphErrBody(code int, details string) map[string]any {
	errObj := map[string]any{
		"message":    "provider fixture error",
		"type":       "OAuthException",
		"code":       code,
		"fbtrace_id": "Az9-fixture-trace",
	}
	if details != "" {
		errObj["error_data"] = map[string]any{"messaging_product": "whatsapp", "details": details}
	}
	return map[string]any{"error": errObj}
}

// fakeGraph is an httptest server standing in for
// graph.facebook.com/v21.0/{phone-number-id}/messages. It captures requests,
// serves scripted replies (queue) or a default success, and — after every
// successful send — optionally invokes onSend on its own goroutine, standing
// in for Meta's asynchronous status webhook callback.
type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []capturedRequest
	queue    []fakeReply
	wamidSeq int
	onSend   func(wamid string)
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	f := &fakeGraph{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) handle(w http.ResponseWriter, r *http.Request) {
	body := make([]byte, 0, r.ContentLength)
	if r.ContentLength > 0 {
		buf := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			f.t.Errorf("fake graph: read request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body = buf
	}
	captured := capturedRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		Auth:        r.Header.Get("Authorization"),
		ContentType: r.Header.Get("Content-Type"),
		RawBody:     body,
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &captured.Body); err != nil {
			f.t.Errorf("fake graph: unparsable request body: %v", err)
		}
	}

	f.mu.Lock()
	f.requests = append(f.requests, captured)
	var reply *fakeReply
	if len(f.queue) > 0 {
		r := f.queue[0]
		f.queue = f.queue[1:]
		reply = &r
	}
	if reply == nil {
		f.wamidSeq++
		wamid := fmt.Sprintf("wamid.fake-%d", f.wamidSeq)
		hook := f.onSend
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messaging_product": "whatsapp",
			"contacts":          []any{map[string]any{"input": "+254711111111", "wa_id": "254711111111"}},
			"messages":          []any{map[string]any{"id": wamid}},
		})
		if hook != nil {
			// Stand-in for Meta's async status callback: the real Graph
			// delivers statuses later, on a different connection. The hook
			// is invoked outside the response path so Send's success is not
			// delayed by (or coupled to) receipt translation.
			go hook(wamid)
		}
		return
	}
	f.mu.Unlock()

	for k, v := range reply.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(reply.status)
	if reply.body != nil {
		_ = json.NewEncoder(w).Encode(reply.body)
	}
}

// enqueue scripts the next responses in order.
func (f *fakeGraph) enqueue(replies ...fakeReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, replies...)
}

// failNext scripts n identical provider failures.
func (f *fakeGraph) failNext(n, status, code int, details string) {
	replies := make([]fakeReply, n)
	for i := range replies {
		replies[i] = fakeReply{status: status, body: graphErrBody(code, details)}
	}
	f.enqueue(replies...)
}

// count reports how many requests the fake has observed.
func (f *fakeGraph) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// requestAt returns the i-th captured request.
func (f *fakeGraph) requestAt(i int) capturedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

// ─── provider construction helpers ──────────────────────────────────────────

// newTestProvider builds a Provider pointed at the fake with the recorder as
// its delivery hook and a recorded (instant) retry sleep.
func newTestProvider(t *testing.T, f *fakeGraph, rec *conformance.Recorder, sleep func(ctx context.Context, d time.Duration) error) *Provider {
	t.Helper()
	if sleep == nil {
		sleep = func(context.Context, time.Duration) error { return nil }
	}
	p, err := New(Config{
		PhoneNumberID: testPhoneNumberID,
		AccessToken:   registry.Secret(testAccessToken),
		// Mirror the production shape: DefaultAPIBaseURL already carries the
		// /v21.0 version segment, so the test base does too.
		APIBaseURL: f.srv.URL + "/v21.0",
		Ingest:     rec.Ingest,
		Signer:     testSigner,
		Retry:      RetryPolicy{Sleep: sleep},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}
