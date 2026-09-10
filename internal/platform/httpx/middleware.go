package httpx

import (
	"context"
	"net/http"
	"sync"
	"time"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/google/uuid"
)

type ctxKey int

const ctxRequestID ctxKey = 100

// RequestIDFrom extracts the request id assigned by middleware.
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// RequestID assigns a request id (honoring an inbound X-Request-ID) and echoes
// it on the response for tracing.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// SecurityHeaders sets the baseline header posture on every response.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Recover converts panics into 500 responses instead of dropped connections.
func Recover(log Logger, next http.Handler) http.Handler {
	return Recoverer(log)(next)
}

// Recoverer is the middleware form of Recover (for chi's Use).
func Recoverer(log Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log("panic recovered", "request_id", RequestIDFrom(r.Context()), "panic", rec)
					WriteError(w, apperrors.Internal("internal.error", "unexpected internal error"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Logger is the minimal logging surface httpx depends on (keeps pkg cycle-free).
type Logger func(msg string, args ...any)

// RateLimit is a token-bucket limiter keyed by arbitrary string (tenant, IP,
// api-key id). Buckets are bounded: at most maxBuckets keys are tracked; the
// oldest half is evicted when the bound is exceeded, so unique-key floods
// cannot grow memory without limit.
type RateLimit struct {
	mu         sync.Mutex
	buckets    map[string]*bucket
	rate       float64 // tokens per second
	burst      float64
	maxBuckets int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimit builds a limiter allowing `burst` events immediately, refilling
// at `perMinute` events/minute, tracking at most maxBuckets keys.
func NewRateLimit(perMinute, burst, maxBuckets int) *RateLimit {
	if perMinute <= 0 {
		perMinute = 60
	}
	if burst <= 0 {
		burst = perMinute
	}
	if maxBuckets <= 0 {
		maxBuckets = 10_000
	}
	return &RateLimit{
		buckets:    make(map[string]*bucket),
		rate:       float64(perMinute) / 60.0,
		burst:      float64(burst),
		maxBuckets: maxBuckets,
	}
}

// Allow consumes one token for key. It reports whether the call is permitted
// and the duration until the next token would be available.
func (rl *RateLimit) Allow(key string, now time.Time) (bool, time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rl.maxBuckets {
			rl.evictLocked(now)
		}
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens -= 1
		return true, 0
	}
	need := (1 - b.tokens) / rl.rate
	return false, time.Duration(need * float64(time.Second))
}

// evictLocked drops the oldest half of buckets by last-touch time.
// Called under lock.
func (rl *RateLimit) evictLocked(now time.Time) {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(rl.buckets))
	for k, b := range rl.buckets {
		all = append(all, kv{k, b.last})
	}
	// insertion-order-independent selection: drop roughly the older half
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].t.Before(all[j-1].t); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	half := len(all) / 2
	if half < 1 {
		half = 1
	}
	for i := 0; i < half; i++ {
		delete(rl.buckets, all[i].k)
	}
}
