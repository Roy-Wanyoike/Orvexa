package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

func TestWriteErrorMapsKinds(t *testing.T) {
	cases := []struct {
		err  *apperrors.Error
		want int
	}{
		{apperrors.Invalid("x.y", "bad"), http.StatusUnprocessableEntity},
		{apperrors.NotFound("x.y", "gone"), http.StatusNotFound},
		{apperrors.Conflict("x.y", "dup"), http.StatusConflict},
		{apperrors.RateLimited("x.y", "slow"), http.StatusTooManyRequests},
		{apperrors.Unauth("x.y", "no"), http.StatusUnauthorized},
		{apperrors.Forbidden("x.y", "nope"), http.StatusForbidden},
		{apperrors.Internal("x.y", "boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		WriteError(w, c.err)
		if w.Code != c.want {
			t.Errorf("kind %s: got %d want %d", c.err.Kind, w.Code, c.want)
		}
	}
}

func TestWriteErrorHidesUnknownInternals(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, errors.New("secret db password leaked"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "secret") {
		t.Fatalf("internal error leaked to client: %s", body)
	}
	if !strings.Contains(body, "internal.error") {
		t.Fatalf("missing stable code: %s", body)
	}
}

func TestRateLimitBurstAndRefill(t *testing.T) {
	rl := NewRateLimit(60, 3, 1000) // 1/s refill, burst 3
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := rl.Allow("k", now); !ok {
			t.Fatalf("burst token %d should pass", i)
		}
	}
	if ok, _ := rl.Allow("k", now); ok {
		t.Fatal("4th token in same instant should be limited")
	}
	// refill at 1 token/sec: after 1s another token is available
	if ok, _ := rl.Allow("k", now.Add(time.Second)); !ok {
		t.Fatal("token after 1s refill should pass")
	}
}

func TestRateLimitKeysAreIndependent(t *testing.T) {
	rl := NewRateLimit(60, 1, 1000)
	now := time.Now()
	if ok, _ := rl.Allow("a", now); !ok {
		t.Fatal("first key should pass")
	}
	if ok, _ := rl.Allow("b", now); !ok {
		t.Fatal("second key should be independent")
	}
}

func TestRateLimitBucketEvictionBoundsMemory(t *testing.T) {
	rl := NewRateLimit(60, 1, 50)
	now := time.Now()
	for i := 0; i < 500; i++ {
		_, _ = rl.Allow(time.Duration(i).String(), now)
	}
	rl.mu.Lock()
	n := len(rl.buckets)
	rl.mu.Unlock()
	if n > 50 {
		t.Fatalf("buckets exceed cap: %d", n)
	}
}

func TestRequestIDEchoedInHeader(t *testing.T) {
	called := false
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if RequestIDFrom(r.Context()) == "" {
			t.Error("request id missing in context")
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("handler not called")
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID not echoed")
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	h := Recover(func(string, ...any) {}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 after panic, got %d", w.Code)
	}
}

// helpers use stdlib strings/errors directly

// ---- security header posture (issue #39 independent sweep) ----

// TestSecurityHeadersOnEveryResponse asserts the full baseline header policy on
// a success path. The middleware is mounted globally in internal/httpserver
// (r.Use), so these must hold for every route in the tree.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/anything", nil))

	want := map[string]string{
		"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Permissions-Policy":        "camera=(), microphone=(), geolocation=()",
		"Cache-Control":             "no-store",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// TestSecurityHeadersOnErrorAndPanicPaths proves the policy survives non-2xx
// handlers and panic recovery (Recoverer writes the response from inside the
// chain, so headers set before next.ServeHTTP must persist).
func TestSecurityHeadersOnErrorAndPanicPaths(t *testing.T) {
	cases := []struct {
		name string
		next http.Handler
	}{
		{"error response", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			WriteError(w, apperrors.NotFound("x.y", "gone"))
		})},
		{"panic recovery", Recoverer(func(string, ...any) {})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			SecurityHeaders(c.next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			if w.Code >= 200 && w.Code < 300 {
				t.Fatalf("expected non-2xx in this case, got %d", w.Code)
			}
			for _, k := range []string{
				"Content-Security-Policy", "Strict-Transport-Security",
				"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy",
			} {
				if w.Header().Get(k) == "" {
					t.Errorf("%s missing on %s", k, c.name)
				}
			}
		})
	}
}

// TestRateLimitEvictionDropsOldestKeys pins the eviction POLICY under
// distinct last-touch timestamps (the realistic case): after the bucket cap
// is exceeded, the least-recently-used half is evicted and the newest keys
// survive. Regression guard for the #39 sweep fix that replaced the O(n²)
// insertion sort with an O(n log n) selection — same policy, bounded cost.
func TestRateLimitEvictionDropsOldestKeys(t *testing.T) {
	rl := NewRateLimit(600, 1, 100)
	base := time.Now()
	for i := 0; i < 100; i++ { // fill to cap, distinct timestamps
		if ok, _ := rl.Allow("warm-"+strconv.Itoa(i), base.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("warm key %d must pass (burst=1/s refill 1... warm has full burst)", i)
		}
	}
	for i := 0; i < 100; i++ { // flood unique keys at later times
		_, _ = rl.Allow("flood-"+strconv.Itoa(i), base.Add(time.Duration(1000+i)*time.Second))
	}
	rl.mu.Lock()
	n := len(rl.buckets)
	_, oldestSurvives := rl.buckets["warm-0"]
	_, newestSurvives := rl.buckets["flood-99"]
	rl.mu.Unlock()
	if n > 100 {
		t.Fatalf("buckets exceed cap: %d", n)
	}
	if oldestSurvives {
		t.Error("oldest warm key should have been evicted")
	}
	if !newestSurvives {
		t.Error("newest flood key should survive eviction")
	}
}

// BenchmarkRateLimitUniqueKeysDistinctTimes is the committed regression
// evidence for the eviction-complexity fix above: measures sustained cost of
// Allow under a unique-key flood at the bucket cap with distinct timestamps.
// Pre-fix: ~150µs/op (O(n²) insertion sort per eviction). Post-fix: sub-µs
// amortized. Run with:
//
//	go test -run XXX -bench RateLimitUniqueKeysDistinctTimes ./internal/platform/httpx/
func BenchmarkRateLimitUniqueKeysDistinctTimes(b *testing.B) {
	rl := NewRateLimit(600, 120, 10_000)
	base := time.Now()
	for i := 0; i < 10_000; i++ {
		rl.Allow("warm-"+strconv.Itoa(i), base.Add(time.Duration(i)*time.Microsecond))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rl.Allow("flood-"+strconv.Itoa(i), base.Add(time.Duration(10_000+i)*time.Microsecond))
	}
}
