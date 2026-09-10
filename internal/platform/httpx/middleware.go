package httpx

import (
	"context"
	"net/http"
	"slices"
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
//
// Mounted globally (internal/httpserver r.Use) BEFORE any handler runs, so the
// policy below holds on every route — 2xx, 4xx/5xx, health, webhooks and
// identity alike — including error responses written by Recoverer.
//
// The API surface is JSON-only (no HTML is served anywhere in the tree), so a
// maximally restrictive Content-Security-Policy is correct and costs nothing:
// default-src 'none' with no allowances. frame-ancestors 'none' is the CSP
// frame-dropping complement to X-Frame-Options: DENY (both are sent because
// legacy clients honor only the latter).
//
// Strict-Transport-Security is sent unconditionally: RFC 6797 §6.1 makes
// clients ignore the header over non-secure transport (harmless in local dev),
// and in the documented production topology TLS terminates at the trusted edge
// proxy, where r.TLS is nil at this handler — gating on r.TLS != nil would
// silently disable HSTS exactly where it matters. max-age covers one year and
// includeSubDomains extends the upgrade promise to every subdomain.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
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
//
// Complexity: O(n log n) per eviction via slices.SortFunc (pdqsort), fired at
// most once per maxBuckets/2 fresh keys — amortized O(log n) per Allow. The
// original hand-rolled insertion sort was O(n²) worst case, which measured
// 150µs/op sustained under a unique-key flood at the 10k-bucket cap (vs
// ~0.4µs steady state): a self-inflicted CPU amplification vector on exactly
// the flood path the bucket bound was meant to defend (issue #39 sweep).
// Policy is unchanged: oldest half by last touch, minimum one eviction.
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
	slices.SortFunc(all, func(a, b kv) int {
		switch {
		case a.t.Before(b.t):
			return -1
		case b.t.Before(a.t):
			return 1
		default:
			return 0
		}
	})
	half := len(all) / 2
	if half < 1 {
		half = 1
	}
	for i := 0; i < half; i++ {
		delete(rl.buckets, all[i].k)
	}
}
