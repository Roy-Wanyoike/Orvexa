// fakeTwilio is the in-process Twilio Programmable Voice double used by the
// adapter tests and the conformance wiring. It models the carrier behaviors
// the adapter's correctness depends on:
//
//   - basic auth with the AccountSID as username (401 envelope on mismatch),
//   - POST /Calls.json -> 201 JSON {"sid": ..., "status": "queued"},
//   - POST /Calls/{Sid}.json -> 200 JSON status update (status changes and
//     TwiML redirects both land here),
//   - native status callbacks delivered ASYNCHRONOUSLY (goroutine, after the
//     REST response) to the exact StatusCallback URL the adapter supplied —
//     the same async shape production Twilio traffic has, which is why the
//     conformance wiring runs with RequireAsyncEvents.
//
// Response scripts make the retry/error-taxonomy tests deterministic, and
// every request/callback the fake sees is captured for assertions.
package twilio

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// fakeResponse is one scripted REST response; a zero status means "behave
// like the real carrier" (201/200 success JSON).
type fakeResponse struct {
	status int               // HTTP status to answer with
	body   string            // raw body; empty + 2xx synthesizes success JSON
	header map[string]string // extra response headers (e.g. Retry-After)
}

// fakePlace is one captured POST /Calls.json attempt (scripted failures
// included — the count is the retry-attempt count).
type fakePlace struct {
	Path string
	Form url.Values
}

// fakeUpdate is one captured POST /Calls/{Sid}.json.
type fakeUpdate struct {
	SID  string
	Form url.Values
}

// fakeCallbackPost is one status callback the fake delivered to the
// adapter's callback endpoint (captured for assertions).
type fakeCallbackPost struct {
	Query url.Values // target URL query (interaction/tenant passthrough)
	Form  url.Values // POSTed body (CallSid, CallStatus, ...)
}

// fakeTwilio is the injectable carrier double. Behavior knobs are guarded
// by mu; capture accessors snapshot under the same lock, so the fake is
// race-clean while its goroutines deliver callbacks.
type fakeTwilio struct {
	server *httptest.Server
	url    string

	accountSID string // expected basic-auth username
	authToken  string // expected basic-auth password

	mu        sync.Mutex
	places    []fakePlace
	updates   []fakeUpdate
	callbacks []fakeCallbackPost

	placedCalls map[string]url.Values // sid -> place form (for callback URLs)
	sids        []string              // sids minted per place attempt, in order

	scriptPlace  []fakeResponse // consumed FIFO per place attempt
	scriptUpdate []fakeResponse // consumed FIFO per update attempt

	handler         http.Handler  // adapter-side receiver bound at /callback
	callbackSeq     []string      // statuses fired after a successful place
	callbackDelay   time.Duration // stagger between async callback posts
	fireOnCompleted bool          // status update Status=completed fires a completed callback
}

// newFakeTwilio starts the fake with production-shaped defaults.
func newFakeTwilio(accountSID, authToken string) *fakeTwilio {
	f := &fakeTwilio{
		accountSID:      accountSID,
		authToken:       authToken,
		placedCalls:     map[string]url.Values{},
		callbackSeq:     []string{"ringing", "in-progress"},
		callbackDelay:   10 * time.Millisecond,
		fireOnCompleted: true,
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	f.url = f.server.URL
	return f
}

// Close shuts the fake down.
func (f *fakeTwilio) Close() { f.server.Close() }

// URL is the fake's base — the adapter's BaseURL and the URL prefixes for
// TwiML/callback configuration.
func (f *fakeTwilio) URL() string { return f.url }

// SetCallbackHandler binds the adapter-side receiver served at /callback
// (the adapter's ServeStatusCallback in the full-lifecycle wiring). Nil
// keeps the endpoint capture-only.
func (f *fakeTwilio) SetCallbackHandler(h http.Handler) {
	f.mu.Lock()
	f.handler = h
	f.mu.Unlock()
}

// SetCallbackSequence overrides the statuses fired after a successful place.
func (f *fakeTwilio) SetCallbackSequence(statuses []string) {
	f.mu.Lock()
	f.callbackSeq = append([]string(nil), statuses...)
	f.mu.Unlock()
}

// SetCallbackDelay overrides the async delivery stagger.
func (f *fakeTwilio) SetCallbackDelay(d time.Duration) {
	f.mu.Lock()
	f.callbackDelay = d
	f.mu.Unlock()
}

// SetFireOnCompleted toggles whether a Status=completed update fires a
// completed callback (the carrier behavior after a hangup).
func (f *fakeTwilio) SetFireOnCompleted(v bool) {
	f.mu.Lock()
	f.fireOnCompleted = v
	f.mu.Unlock()
}

// ScriptPlace queues responses consumed by consecutive place attempts.
func (f *fakeTwilio) ScriptPlace(responses ...fakeResponse) {
	f.mu.Lock()
	f.scriptPlace = append(f.scriptPlace, responses...)
	f.mu.Unlock()
}

// ScriptUpdate queues responses consumed by consecutive update attempts.
func (f *fakeTwilio) ScriptUpdate(responses ...fakeResponse) {
	f.mu.Lock()
	f.scriptUpdate = append(f.scriptUpdate, responses...)
	f.mu.Unlock()
}

// Places snapshots every captured place attempt.
func (f *fakeTwilio) Places() []fakePlace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePlace(nil), f.places...)
}

// Updates snapshots every captured update attempt.
func (f *fakeTwilio) Updates() []fakeUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeUpdate(nil), f.updates...)
}

// Callbacks snapshots every delivered status callback.
func (f *fakeTwilio) Callbacks() []fakeCallbackPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCallbackPost(nil), f.callbacks...)
}

// CallSID returns the sid the fake minted for the nth place attempt (-1 =
// last).
func (f *fakeTwilio) CallSID(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 0 {
		n = len(f.places) - 1
	}
	if n < 0 || n >= len(f.sids) {
		return ""
	}
	return f.sids[n]
}

// serve routes by resource shape (the paths the adapter's client builds).
func (f *fakeTwilio) serve(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/Calls.json"):
		f.servePlace(w, r)
	case strings.Contains(r.URL.Path, "/Calls/") && strings.HasSuffix(r.URL.Path, ".json"):
		f.serveUpdate(w, r)
	case r.URL.Path == "/callback":
		f.serveCallback(w, r)
	default:
		http.NotFound(w, r)
	}
}

// checkAuth enforces Twilio's documented basic-auth scheme and answers with
// the native 401 envelope on mismatch — the adapter's auth-failure taxonomy
// test rides on this path.
func (f *fakeTwilio) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if ok && user == f.accountSID && pass == f.authToken {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":      20003,
		"message":   "Authenticate",
		"more_info": "https://www.twilio.com/docs/errors/20003",
		"status":    401,
	})
	return false
}

// servePlace handles POST /Calls.json: capture, consume an optional script
// entry, answer, and — on success — fire the status-callback sequence
// asynchronously to the StatusCallback URL the adapter supplied.
func (f *fakeTwilio) servePlace(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.places = append(f.places, fakePlace{Path: r.URL.Path, Form: cloneValues(r.PostForm)})
	scripted := fakeResponse{}
	if len(f.scriptPlace) > 0 {
		scripted = f.scriptPlace[0]
		f.scriptPlace = f.scriptPlace[1:]
	}
	callbackTarget := r.PostForm.Get("StatusCallback")
	seq := append([]string(nil), f.callbackSeq...)
	delay := f.callbackDelay
	f.mu.Unlock()

	status := scripted.status
	if status == 0 {
		status = http.StatusCreated
	}
	sid := mintSID()
	f.mu.Lock()
	f.sids = append(f.sids, sid)
	f.mu.Unlock()

	body := scripted.body
	if body == "" && status >= 200 && status < 300 {
		bodyBytes, _ := json.Marshal(map[string]string{"sid": sid, "status": "queued"})
		body = string(bodyBytes)
	}
	for k, v := range scripted.header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))

	if status >= 200 && status < 300 {
		f.mu.Lock()
		f.placedCalls[sid] = cloneValues(r.PostForm)
		f.mu.Unlock()
		go f.fireSequence(sid, callbackTarget, seq, delay)
	}
}

// serveUpdate handles POST /Calls/{Sid}.json: status changes (hangup) and
// TwiML redirects (transfer/hold/resume) both land here. TwiML redirects
// produce no status callbacks (the leg's CallStatus stays in-progress —
// why Transfer announces its ringing phase itself), while Status=completed
// fires the completed callback the way the real carrier does.
func (f *fakeTwilio) serveUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	sid := sidFromPath(r.URL.Path)
	f.mu.Lock()
	f.updates = append(f.updates, fakeUpdate{SID: sid, Form: cloneValues(r.PostForm)})
	scripted := fakeResponse{}
	if len(f.scriptUpdate) > 0 {
		scripted = f.scriptUpdate[0]
		f.scriptUpdate = f.scriptUpdate[1:]
	}
	callbackTarget := ""
	if orig, ok := f.placedCalls[sid]; ok {
		callbackTarget = orig.Get("StatusCallback")
	}
	fireCompleted := f.fireOnCompleted && r.PostForm.Get("Status") == "completed"
	delay := f.callbackDelay
	f.mu.Unlock()

	status := scripted.status
	if status == 0 {
		status = http.StatusOK
	}
	body := scripted.body
	if body == "" && status >= 200 && status < 300 {
		bodyBytes, _ := json.Marshal(map[string]string{"sid": sid, "status": r.PostForm.Get("Status")})
		body = string(bodyBytes)
	}
	for k, v := range scripted.header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))

	if fireCompleted {
		go f.fireSequence(sid, callbackTarget, []string{"completed"}, delay)
	}
}

// serveCallback is the adapter-side receiver at /callback: the fake records
// the delivery and forwards the request verbatim to the registered handler.
func (f *fakeTwilio) serveCallback(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.callbacks = append(f.callbacks, fakeCallbackPost{
		Query: cloneValues(r.URL.Query()),
		Form:  cloneValues(r.PostForm),
	})
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h.ServeHTTP(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// fireSequence delivers a status-callback sequence the way the real carrier
// does: asynchronously, one form-encoded POST per status to the exact
// StatusCallback URL (self-scoping query string included).
func (f *fakeTwilio) fireSequence(sid, callbackURL string, statuses []string, delay time.Duration) {
	for _, st := range statuses {
		if delay > 0 {
			time.Sleep(delay)
		}
		if !f.postCallback(sid, callbackURL, st) {
			return
		}
	}
}

// postCallback delivers one status callback; a missing target or a refused
// connection ends the sequence (the fake never spins).
func (f *fakeTwilio) postCallback(sid, callbackURL, status string) bool {
	if strings.TrimSpace(callbackURL) == "" {
		return false
	}
	form := url.Values{
		"CallSid":    {sid},
		"CallStatus": {status},
		"AccountSid": {f.accountSID},
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.PostForm(callbackURL, form)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return true
}

// sidFromPath extracts the sid from /.../Calls/{Sid}.json.
func sidFromPath(path string) string {
	i := strings.LastIndex(path, "/Calls/")
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(path[i+len("/Calls/"):], ".json")
}

// cloneValues deep-copies a form for lock-safe capture.
func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// mintSID produces Twilio-shaped call sids (CA + 32 hex) so the adapter
// round-trips a realistic identifier.
func mintSID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "CA00000000000000000000000000000000"
	}
	return "CA" + hex.EncodeToString(b[:])
}
