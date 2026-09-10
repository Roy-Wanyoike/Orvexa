package twilio

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Roy-Wanyoike/orvexa/internal/comms/registry"
	"github.com/Roy-Wanyoike/orvexa/internal/messaging"
)

// fakePost is one captured /Messages.json request.
type fakePost struct {
	path     string
	authUser string
	authPass string
	form     url.Values
}

// fakeStatus is one scripted API response: status code plus optional
// Retry-After guidance for 429s.
type fakeStatus struct {
	status     int
	retryAfter string
}

// fakeTwilio is the httptest stand-in for Twilio's REST API plus a receiver
// for the status callbacks the adapter configures. The API server records
// every POST (path, basic-auth pair, form) and plays a scripted response
// sequence. On success it simulates Twilio's async delivery reporting by
// POSTing form-encoded status callbacks to the StatusCallback URL the
// adapter set — through REAL http, so tests prove the full wire path.
type fakeTwilio struct {
	apiSrv *httptest.Server
	cbSrv  *httptest.Server
	cbBase string // value tests hand the adapter as StatusCallbackBase

	mu        sync.Mutex
	posts     []fakePost
	script    []fakeStatus
	cbFired   []url.Values
	sink      func(context.Context, url.Values) error // callback receiver (wired by translation tests)
	fireDelay time.Duration
	sidSeq    int
}

func newFakeTwilio(t *testing.T) *fakeTwilio {
	t.Helper()
	f := &fakeTwilio{fireDelay: 2 * time.Millisecond}
	f.apiSrv = httptest.NewServer(http.HandlerFunc(f.handleAPI))
	f.cbSrv = httptest.NewServer(http.HandlerFunc(f.handleCallback))
	f.cbBase = f.cbSrv.URL + "/webhooks/twilio/status"
	t.Cleanup(func() {
		f.apiSrv.Close()
		f.cbSrv.Close()
	})
	return f
}

// setSink wires the receiver that fired status callbacks are handed to (the
// translation tests pass the adapter's HandleStatusCallback method value).
func (f *fakeTwilio) setSink(fn func(context.Context, url.Values) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sink = fn
}

// script queues API responses (front first). An empty script means 201.
func (f *fakeTwilio) scriptResponses(statuses ...fakeStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, statuses...)
}

// resetScript clears any queued responses.
func (f *fakeTwilio) resetScript() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = nil
}

// postCount returns how many API POSTs were captured.
func (f *fakeTwilio) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// lastPost returns the most recent captured POST.
func (f *fakeTwilio) lastPost() fakePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) == 0 {
		return fakePost{}
	}
	return f.posts[len(f.posts)-1]
}

// firedCallbacks returns the callback forms observed by the callback server
// (form body and callback-URL query merged, exactly what ParseForm yields).
func (f *fakeTwilio) firedCallbacks() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]url.Values, len(f.cbFired))
	copy(out, f.cbFired)
	return out
}

func (f *fakeTwilio) handleAPI(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	user, pass, _ := r.BasicAuth()

	f.mu.Lock()
	f.posts = append(f.posts, fakePost{
		path:     r.URL.Path,
		authUser: user,
		authPass: pass,
		form:     cloneValues(r.PostForm),
	})
	status := http.StatusCreated
	var retryAfter string
	if len(f.script) > 0 {
		s := f.script[0]
		f.script = f.script[1:]
		status = s.status
		retryAfter = s.retryAfter
	}
	cb := f.posts[len(f.posts)-1].form.Get("StatusCallback")
	f.mu.Unlock()

	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusCreated {
		f.mu.Lock()
		f.sidSeq++
		sid := fmt.Sprintf("SMfake%06d", f.sidSeq)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"sid":"` + sid + `","status":"queued"}`))
		if cb != "" {
			go f.fireCallbacks(cb, sid)
		}
		return
	}
	_, _ = w.Write([]byte(`{"code":21211,"message":"The 'To' number is not a valid phone number","more_info":"https://www.twilio.com/docs/errors/21211","status":400}`))
}

// fireCallbacks simulates Twilio's async delivery reporting: form-encoded
// status callbacks over real HTTP to the adapter-configured callback URL,
// first "sent" then "delivered".
func (f *fakeTwilio) fireCallbacks(cb, sid string) {
	time.Sleep(f.fireDelay)
	for _, st := range []string{"sent", "delivered"} {
		form := url.Values{
			"MessageSid":    {sid},
			"MessageStatus": {st},
			"SmsStatus":     {st},
			"To":            {"%2B254711111111"},
			"From":          {"+15005550006"},
		}
		resp, err := f.cbSrv.Client().PostForm(cb, form)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
		}
		time.Sleep(f.fireDelay)
	}
}

func (f *fakeTwilio) handleCallback(w http.ResponseWriter, r *http.Request) {
	// ParseForm merges the POST body and the callback-URL query — the merged
	// view TranslateStatus expects (routing keys ride the URL).
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.cbFired = append(f.cbFired, cloneValues(r.Form))
	sink := f.sink
	f.mu.Unlock()

	if sink == nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err := sink(r.Context(), r.Form); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// sleepRecorder captures retry backoff sleeps (the adapter's test seam).
type sleepRecorder struct {
	mu  sync.Mutex
	got []time.Duration
}

func (s *sleepRecorder) record(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, d)
}

func (s *sleepRecorder) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.got...)
}

// validMsg returns a fully valid outbound message the tests mutate.
func validMsg(tenantID string) messaging.Message {
	return messaging.Message{
		InteractionID: uuid.NewString(),
		TenantID:      tenantID,
		Channel:       messaging.ChannelSMS,
		From:          "+15005550006",
		To:            "+254 711 111 111",
		Body:          "orvexa twilio adapter test",
	}
}

// testAccountSID / testAuthToken are obviously-fake credential fixtures.
// They exist to prove they never leak into renderings, errors or logs.
const (
	testAccountSID = "AC0123456789abcdef0123456789abcdef"
	testAuthToken  = "conformance-fake-auth-token-9876"
)

// newTestAdapter builds an adapter pointed at the fake API server.
func newTestAdapter(t *testing.T, f *fakeTwilio, mutate func(*Config)) *Adapter {
	t.Helper()
	cfg := Config{
		AccountSID:         registry.Secret(testAccountSID),
		AuthToken:          registry.Secret(testAuthToken),
		FromNumber:         registry.Secret("+15005550006"),
		APIBaseURL:         f.apiSrv.URL,
		StatusCallbackBase: f.cbBase,
		RetryBackoff:       time.Millisecond,
		RetryMaxBackoff:    4 * time.Millisecond,
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

// cloneValues deep-copies a form for mutex-guarded storage.
func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// sleepSeam installs a recording sleep on the adapter (returns the recorder).
func sleepSeam(a *Adapter) *sleepRecorder {
	rec := &sleepRecorder{}
	a.sleep = rec.record
	return rec
}
