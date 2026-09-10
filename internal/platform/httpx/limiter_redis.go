// Redis-backed distributed rate limiter — issue #37 [O-28] (additive driver
// file; middleware.go is untouched and remains the default when
// ORVEXA_REDIS_URL is unset, byte-identical to the pre-#37 behavior).
//
// What is implemented, documented honestly and enforced by tests:
//
//   - The SAME method surface as the in-process token bucket:
//     Allow(key string, now time.Time) (bool, time.Duration). Both drivers
//     satisfy the Limiter interface below, so wiring can swap them by env
//     without touching any middleware.
//
//   - A server-side FIXED window per bucket: every Allow issues INCR on
//     `orvexa:rl:v1:<scope>:<key>:<bucket>` followed by EXPIRE NX with the
//     window as TTL. `bucket` is derived from the caller-supplied `now`
//     (now.UnixNano() / window), so every node sharing Redis lands on the
//     same bucket without a round trip to Redis TIME. The issue text calls
//     this a sliding window — precisely, it is a fixed window per aligned
//     bucket; the honest consequence (up to 2× limit across a bucket
//     boundary) is stated below and asserted in tests.
//
//   - Server-side keying (tenant + route): the interface hands the driver a
//     single opaque key — today the API key id on the authenticated surface
//     (tenancy middleware) and the client IP on the webhook ingress. The
//     route dimension is supplied at construction as `scope` (e.g. "auth",
//     "webhooks"), producing keys `orvexa:rl:v1:<scope>:<key>:<bucket>`.
//     State therefore lives in Redis keyed by tenant principal + route class
//     instead of a process-local map, which is what makes limits global per
//     tenant across a multi-binary deployment. Per-HTTP-route granularity
//     would require middleware to pass the route pattern per call — a
//     consumer-side change outside this wave.
//
//   - Token-bucket divergences from *RateLimit (both intended, both
//     documented): a fixed window has no refill-rate/burst split — the cap
//     is `Limit` events per aligned `Window` for every caller; maxBuckets is
//     meaningless (Redis bounds memory via TTL eviction); denied attempts
//     still INCR (counts can overshoot the limit; they are never throttled
//     back below it). Equivalence with the in-process driver is proven for
//     the burst-at-window-start pattern in the tests.
//
// Fail semantics — implemented exactly as specified and asserted in tests:
//
//   - FailClosed=false (default, "open elsewhere"): any Redis error (dial,
//     command, timeout) → Allow returns (true, 0) — the request proceeds
//     unaudited by any limiter for that call, and the outage is visible via
//     `redis_rate_limit_error` logs per operation.
//   - FailClosed=true (auth-critical surfaces, e.g. the tenancy middleware
//     that guards every authenticated route): any Redis error → Allow
//     returns (false, remaining window) — the middleware answers 429 with a
//     Retry-After, i.e. during a Redis outage the route stops serving
//     instead of serving unbounded.
//   - There is NO silent in-process fallback limiter: a hybrid policy would
//     make allow/deny node-dependent and untestable. Operators needing a
//     bounded-degradation floor should wire the process-local driver
//     alongside (wiring is the glue wave's scope).
//   - Errors are logged with the operation name only — never the composed
//     key (it embeds tenant principals), the URL, or credentials.
//
// Env: ORVEXA_REDIS_URL (e.g. redis://localhost:6379/0). NewRateLimitFromEnv
// returns the existing in-process token bucket when it is unset.
package httpx

import (
	"context"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter is the method surface middleware consumes — the exact signature of
// (*RateLimit).Allow — so the in-process and Redis drivers are interchangeable.
type Limiter interface {
	Allow(key string, now time.Time) (bool, time.Duration)
}

// Compile-time swap contract: both drivers implement the same surface.
var (
	_ Limiter = (*RateLimit)(nil)
	_ Limiter = (*RedisRateLimit)(nil)
)

const (
	redisRateLimitPrefix = "orvexa:rl:v1:"
	// redisRateLimitOpTimeout bounds each Redis call: Allow runs on the
	// request hot path and the Limiter interface carries no context, so an
	// unbounded call would stall requests exactly when Redis is unhealthy.
	redisRateLimitOpTimeout = 500 * time.Millisecond
	// redisRateLimitBootTimeout bounds the startup Ping in NewRateLimitFromEnv.
	redisRateLimitBootTimeout = 2 * time.Second
	redisRateLimitDefaultWin  = time.Minute
	redisRateLimitDefaultCap  = 60
)

// RedisRateLimitConfig configures the Redis driver.
type RedisRateLimitConfig struct {
	// Limit is the maximum allowed events per caller per aligned Window.
	// <= 0 defaults to 60 (the in-process driver's perMinute default).
	Limit int
	// Window is the fixed-window length. <= 0 defaults to 1 minute.
	Window time.Duration
	// Scope namespaces the keys by route class (tenant + route keying).
	// Empty defaults to "default" — set it explicitly when mounting more
	// than one limiter so route classes never share a budget.
	Scope string
	// FailClosed selects the outage policy: false → allow on Redis error
	// (open elsewhere); true → deny on Redis error (auth-critical routes).
	FailClosed bool
	// Log receives one line per degraded Redis operation. Never keyed by
	// tenant material; nil is allowed and discards.
	Log Logger
}

// RedisRateLimit implements Limiter with state in Redis (INCR + EXPIRE NX
// fixed window), making the limit global per caller across every node that
// shares the Redis. Safe for concurrent use; immutable after construction.
type RedisRateLimit struct {
	client     redis.UniversalClient
	limit      int64
	window     time.Duration
	scope      string
	failClosed bool
	log        Logger // never nil after construction
}

// NewRedisRateLimit builds the Redis-backed driver over an existing client.
// The caller retains ownership of client (close it via the cleanup returned
// by NewRateLimitFromEnv, or their own wiring).
func NewRedisRateLimit(client redis.UniversalClient, cfg RedisRateLimitConfig) *RedisRateLimit {
	if cfg.Limit <= 0 {
		cfg.Limit = redisRateLimitDefaultCap
	}
	if cfg.Window <= 0 {
		cfg.Window = redisRateLimitDefaultWin
	}
	if cfg.Scope == "" {
		cfg.Scope = "default"
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &RedisRateLimit{
		client:     client,
		limit:      int64(cfg.Limit),
		window:     cfg.Window,
		scope:      cfg.Scope,
		failClosed: cfg.FailClosed,
		log:        cfg.Log,
	}
}

// Allow implements Limiter. Happy path: INCR the caller's bucket; a count
// within the limit is allowed, anything above is denied with the time until
// the bucket rotates (the middleware clamps Retry-After to >= 1s). A fresh
// bucket also gets EXPIRE NX with the window as TTL; the EXPIRE result never
// changes the decision — a lost EXPIRE is retried by the next call in the
// same bucket, and the time-derived bucket name bounds the damage of an
// orphaned key to a single window. Redis errors follow the configured fail
// policy (see the package-level documentation above).
func (r *RedisRateLimit) Allow(key string, now time.Time) (bool, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), redisRateLimitOpTimeout)
	defer cancel()

	rkey := r.key(key, now)
	n, err := r.client.Incr(ctx, rkey).Result()
	if err != nil {
		return r.fail("incr", err)
	}
	// EXPIRE NX: stamps a fresh (or orphaned, TTL-less) counter without ever
	// shortening an existing TTL. A failure here cannot flip the decision —
	// the INCR already happened and n is authoritative.
	if _, err := r.client.ExpireNX(ctx, rkey, r.window).Result(); err != nil {
		r.log("redis_rate_limit_error", "op", "expire_nx", "err", err)
	}
	if n > r.limit {
		// Capacity returns when the bucket rotates, not when the TTL lapses.
		bucket := now.UnixNano() / r.window.Nanoseconds()
		remaining := time.Duration((bucket+1)*r.window.Nanoseconds() - now.UnixNano())
		if remaining < 0 {
			remaining = 0
		}
		return false, remaining
	}
	return true, 0
}

// fail applies the documented outage policy. The operation name is logged;
// the key, URL and credentials never are.
func (r *RedisRateLimit) fail(op string, err error) (bool, time.Duration) {
	r.log("redis_rate_limit_error", "op", op, "err", err)
	if r.failClosed {
		return false, r.window // Retry-After: the outage may end within a window
	}
	return true, 0
}

// key composes the server-side key: tenant-or-principal (the caller's opaque
// key) + route class (scope) + aligned time bucket.
func (r *RedisRateLimit) key(key string, now time.Time) string {
	bucket := now.UnixNano() / r.window.Nanoseconds()
	return redisRateLimitPrefix + r.scope + ":" + key + ":" + strconv.FormatInt(bucket, 10)
}

// NewRateLimitFromEnv is the env-gated swap point for the limiter family.
//
//   - ORVEXA_REDIS_URL unset → (NewRateLimit(perMinute, burst, maxBuckets),
//     noop cleanup, nil): the exact in-process token bucket, byte-identical
//     to the pre-#37 default path.
//   - Set but malformed → error. Operator intent is explicit, so
//     misconfiguration is never silently swallowed; the configured value is
//     NOT echoed (credential hygiene).
//   - Set but unreachable → the Redis driver is still returned; every Allow
//     then follows the configured fail policy (open or closed) until Redis
//     recovers, per the documented semantics.
//
// When Redis is enabled, cfg.Limit is the fixed-window cap. perMinute/burst/
// maxBuckets only shape the in-process driver returned when the env is
// unset; pass the same numbers for both to keep the policy drift-free.
func NewRateLimitFromEnv(ctx context.Context, cfg RedisRateLimitConfig, perMinute, burst, maxBuckets int) (Limiter, func(), error) {
	url := os.Getenv("ORVEXA_REDIS_URL")
	if url == "" {
		return NewRateLimit(perMinute, burst, maxBuckets), func() {}, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, nil, errors.New("ORVEXA_REDIS_URL is not a valid redis URL (expected redis://[user:pass@]host:port[/db]); the configured value is not echoed")
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, redisRateLimitBootTimeout)
	pingErr := client.Ping(pingCtx).Err()
	cancel()
	if pingErr != nil {
		// Dial errors carry host:port only — never credentials.
		if cfg.Log != nil {
			cfg.Log("redis_rate_limit_unreachable_at_boot", "err", pingErr)
		}
	}
	return NewRedisRateLimit(client, cfg), func() { _ = client.Close() }, nil
}
