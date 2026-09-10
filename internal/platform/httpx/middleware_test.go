package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
