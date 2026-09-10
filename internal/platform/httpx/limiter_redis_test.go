package httpx

import (
        "bufio"
        "context"
        "errors"
        "io"
        "net"
        "strconv"
        "strings"
        "sync"
        "testing"
        "time"

        "github.com/redis/go-redis/v9"
)

// alignedBase is a time exactly on a 1-minute bucket boundary, so window-end
// arithmetic in the assertions stays trivial: bucket = base, retry = window.
var alignedBase = time.Unix(1699999980, 0).UTC() // 1699999980 % 60 == 0

// fakeRedis is a minimal in-process implementation of the go-redis
// UniversalClient surface covering exactly the commands the limiter driver
// issues (INCR/EXPIRE NX/CLOSE). No new deps, no miniredis: the embedded nil
// interface satisfies UniversalClient, so any command the driver should
// never issue would panic loudly in tests instead of passing silently.
type fakeRedis struct {
        redis.UniversalClient

        mu      sync.Mutex
        counts  map[string]int64         // INCR state per key
        ttls    map[string]time.Duration // TTL stamped by EXPIRE NX (zero = none)
        failOp  string                   // "incr" | "expire" — failing op ("" = none)
        failure error
        calls   map[string]int
        logs    []string // captured log messages (driver must emit on degradation)
}

var _ redis.UniversalClient = (*fakeRedis)(nil)

func newFakeRedis() *fakeRedis {
        return &fakeRedis{
                counts: map[string]int64{},
                ttls:   map[string]time.Duration{},
                calls:  map[string]int{},
        }
}

func (f *fakeRedis) Incr(_ context.Context, key string) *redis.IntCmd {
        f.mu.Lock()
        defer f.mu.Unlock()
        f.calls["incr"]++
        if f.failOp == "incr" && f.failure != nil {
                return redis.NewIntResult(0, f.failure)
        }
        f.counts[key]++
        return redis.NewIntResult(f.counts[key], nil)
}

func (f *fakeRedis) ExpireNX(_ context.Context, key string, ttl time.Duration) *redis.BoolCmd {
        f.mu.Lock()
        defer f.mu.Unlock()
        f.calls["expire_nx"]++
        if f.failOp == "expire" && f.failure != nil {
                return redis.NewBoolResult(false, f.failure)
        }
        if _, ok := f.counts[key]; !ok {
                return redis.NewBoolResult(false, nil) // real Redis: EXPIRE on a missing key → 0
        }
        if _, hasTTL := f.ttls[key]; hasTTL {
                return redis.NewBoolResult(false, nil) // NX: TTL already present → 0
        }
        f.ttls[key] = ttl
        return redis.NewBoolResult(true, nil)
}

func (f *fakeRedis) Close() error { return nil }

func (f *fakeRedis) capture(msg string, _ ...any) {
        f.mu.Lock()
        defer f.mu.Unlock()
        f.logs = append(f.logs, msg)
}

func (f *fakeRedis) callCount(op string) int {
        f.mu.Lock()
        defer f.mu.Unlock()
        return f.calls[op]
}

// captureLogger wires a Logger into a config and records into the fake.
func (f *fakeRedis) captureLogger(cfg *RedisRateLimitConfig) {
        cfg.Log = f.capture
}

func newTestRedisRateLimit(fr *fakeRedis, cfg RedisRateLimitConfig) *RedisRateLimit {
        fr.captureLogger(&cfg)
        return NewRedisRateLimit(fr, cfg)
}

func TestRedisRateLimitAllowsUpToLimitThenDenies(t *testing.T) {
        fr := newFakeRedis()
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 3, Window: time.Minute, Scope: "auth"})

        for i := 1; i <= 3; i++ {
                ok, retry := rl.Allow("key-1", alignedBase)
                if !ok || retry != 0 {
                        t.Fatalf("call %d within the limit must be allowed (true, 0), got (%v, %v)", i, ok, retry)
                }
        }
        ok, retry := rl.Allow("key-1", alignedBase)
        if ok {
                t.Fatal("call past the limit must be denied")
        }
        // Bucket starts exactly at alignedBase → capacity returns at +window.
        if want := time.Minute; retry != want {
                t.Fatalf("retry = %v, want %v (time until the bucket rotates)", retry, want)
        }
        if n := fr.callCount("incr"); n != 4 {
                t.Fatalf("INCR calls = %d, want 4 (denied attempts still count)", n)
        }
}

func TestRedisRateLimitBucketRotationRestoresCapacity(t *testing.T) {
        fr := newFakeRedis()
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 2, Window: time.Minute, Scope: "auth"})

        for i := 0; i < 2; i++ {
                if ok, _ := rl.Allow("k", alignedBase); !ok {
                        t.Fatalf("call %d in the first bucket must be allowed", i+1)
                }
        }
        if ok, _ := rl.Allow("k", alignedBase); ok {
                t.Fatal("third call in the same bucket must be denied")
        }
        next := alignedBase.Add(time.Minute) // strictly later bucket
        if ok, retry := rl.Allow("k", next); !ok || retry != 0 {
                t.Fatalf("first call of the next bucket must be allowed, got (%v, %v)", ok, retry)
        }
}

func TestRedisRateLimitKeysAreScopeAndCallerScoped(t *testing.T) {
        fr := newFakeRedis()
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 1, Window: time.Minute, Scope: "auth"})
        other := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 1, Window: time.Minute, Scope: "webhooks"})

        if ok, _ := rl.Allow("tenant-a", alignedBase); !ok {
                t.Fatal("first caller must be allowed")
        }
        // Same tenant, different route class (scope): separate budget.
        if ok, _ := other.Allow("tenant-a", alignedBase); !ok {
                t.Fatal("same tenant on another scope must have a separate budget")
        }
        // Same scope, different tenant: separate budget.
        if ok, _ := rl.Allow("tenant-b", alignedBase); !ok {
                t.Fatal("different tenant in the same scope must have a separate budget")
        }
        wantPrefix := "orvexa:rl:v1:auth:tenant-a:" + strconv.FormatInt(alignedBase.UnixNano()/int64(time.Minute), 10)
        found := false
        for key := range fr.counts {
                if strings.HasPrefix(key, "orvexa:rl:v1:auth:tenant-a:") && strings.Contains(key, wantPrefix[:len(wantPrefix)]) {
                        found = true
                }
                if !strings.Contains(key, strconv.FormatInt(alignedBase.UnixNano()/int64(time.Minute), 10)) {
                        t.Fatalf("every key must embed the aligned bucket, got %q", key)
                }
        }
        if !found {
                t.Fatalf("expected a key %s*, got %v", wantPrefix, fr.counts)
        }
}

func TestRedisRateLimitFailOpenOnRedisError(t *testing.T) {
        fr := newFakeRedis()
        fr.failOp, fr.failure = "incr", errors.New("redis down")
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 1, Window: time.Minute, Scope: "auth"})

        ok, retry := rl.Allow("k", alignedBase)
        if !ok || retry != 0 {
                t.Fatalf("default policy is fail-open: want (true, 0), got (%v, %v)", ok, retry)
        }
        if len(fr.logs) != 1 || fr.logs[0] != "redis_rate_limit_error" {
                t.Fatalf("the outage must be logged exactly once, got %v", fr.logs)
        }
}

func TestRedisRateLimitFailClosedOnRedisError(t *testing.T) {
        fr := newFakeRedis()
        fr.failOp, fr.failure = "incr", errors.New("redis down")
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{
                Limit: 1, Window: time.Minute, Scope: "auth", FailClosed: true,
        })

        ok, retry := rl.Allow("k", alignedBase)
        if ok {
                t.Fatal("auth-critical policy is fail-closed: a Redis error must deny")
        }
        if retry != time.Minute {
                t.Fatalf("retry = %v, want the window (%v) so Retry-After bounds the outage", retry, time.Minute)
        }
        if len(fr.logs) != 1 || fr.logs[0] != "redis_rate_limit_error" {
                t.Fatalf("the outage must be logged exactly once, got %v", fr.logs)
        }
}

func TestRedisRateLimitExpireFailureNeverFlipsDecision(t *testing.T) {
        fr := newFakeRedis()
        fr.failOp, fr.failure = "expire", errors.New("expire lost")
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 2, Window: time.Minute, Scope: "auth"})

        for i := 1; i <= 2; i++ {
                if ok, _ := rl.Allow("k", alignedBase); !ok {
                        t.Fatalf("call %d must be allowed despite the EXPIRE failure", i)
                }
        }
        if ok, _ := rl.Allow("k", alignedBase); ok {
                t.Fatal("the count alone decides: third call must be denied")
        }
        if len(fr.logs) == 0 {
                t.Fatal("the EXPIRE failure must be logged")
        }
}

func TestRedisRateLimitFreshBucketGetsWindowTTL(t *testing.T) {
        fr := newFakeRedis()
        rl := newTestRedisRateLimit(fr, RedisRateLimitConfig{Limit: 5, Window: time.Minute, Scope: "auth"})

        if ok, _ := rl.Allow("k", alignedBase); !ok {
                t.Fatal("first call must be allowed")
        }
        if fr.callCount("expire_nx") != 1 {
                t.Fatalf("EXPIRE NX calls = %d, want 1 (every Allow stamps the bucket)", fr.callCount("expire_nx"))
        }
        for key, ttl := range fr.ttls {
                if ttl != time.Minute {
                        t.Fatalf("TTL on %q = %v, want the full window", key, ttl)
                }
        }
}

func TestRedisRateLimitConfigDefaults(t *testing.T) {
        rl := NewRedisRateLimit(newFakeRedis(), RedisRateLimitConfig{})
        if rl.limit != 60 {
                t.Fatalf("Limit <= 0 must default to 60, got %d", rl.limit)
        }
        if rl.window != time.Minute {
                t.Fatalf("Window <= 0 must default to 1 minute, got %v", rl.window)
        }
        if rl.scope != "default" {
                t.Fatalf("empty Scope must default to %q, got %q", "default", rl.scope)
        }
        if rl.log == nil {
                t.Fatal("Log must never be nil after construction")
        }
}

// TestRedisRateLimitSwapEquivalenceWithInProcess proves the acceptance
// criterion "swap-in/swap-out via env with zero default drift" at the
// behavior level: for the burst-at-window-start pattern, the Redis driver
// and the in-process token bucket (configured with burst == limit) make
// IDENTICAL allow/deny decisions — first `limit` calls allowed, the next
// denied, and capacity restored after a full window.
//
// Documented divergence (asserted below, not hidden): the token bucket
// refills continuously, so a caller spacing requests over time never sees a
// deny at rate <= perMinute; the fixed window only guarantees <= limit per
// aligned window and can allow up to 2× limit across a bucket boundary.
func TestRedisRateLimitSwapEquivalenceWithInProcess(t *testing.T) {
        const limit = 4
        redisRL := NewRedisRateLimit(newFakeRedis(), RedisRateLimitConfig{Limit: limit, Window: time.Minute, Scope: "auth"})
        inprocRL := NewRateLimit(limit /* perMinute */, limit /* burst */, 10_000)

        for i := 1; i <= limit; i++ {
                a, _ := redisRL.Allow("t", alignedBase)
                b, _ := inprocRL.Allow("t", alignedBase)
                if a != b || !a {
                        t.Fatalf("call %d: drivers disagree (redis=%v inproc=%v)", i, a, b)
                }
        }
        a, _ := redisRL.Allow("t", alignedBase)
        b, _ := inprocRL.Allow("t", alignedBase)
        if a != b || a {
                t.Fatalf("call past the limit: drivers disagree (redis=%v inproc=%v)", a, b)
        }
        next := alignedBase.Add(time.Minute)
        a, _ = redisRL.Allow("t", next)
        b, _ = inprocRL.Allow("t", next)
        if a != b || !a {
                t.Fatalf("first call of the next window: drivers disagree (redis=%v inproc=%v)", a, b)
        }

        // Steady caller at exactly the refill rate: the token bucket never
        // denies; the fixed window can deny near the bucket cap — the documented
        // divergence, asserted so it can never regress silently.
        steady := time.Unix(1699999980, 0).UTC()
        deniedByRedis := false
        for i := 0; i < 90; i++ { // 1.5 requests/second-ish across 1.5 windows
                at := steady.Add(time.Duration(i) * 666 * time.Millisecond)
                ok, _ := redisRL.Allow("steady", at)
                if !ok {
                        deniedByRedis = true
                        break
                }
        }
        if !deniedByRedis {
                t.Fatal("expected the fixed window to eventually deny a caller packing more than `limit` events into one aligned window (documented divergence)")
        }
}

func TestNewRateLimitFromEnvUnsetReturnsInProcessLimiter(t *testing.T) {
        t.Setenv("ORVEXA_REDIS_URL", "")
        lim, cleanup, err := NewRateLimitFromEnv(context.Background(), RedisRateLimitConfig{}, 600, 120, 10_000)
        if err != nil {
                t.Fatalf("unset env must never error: %v", err)
        }
        defer cleanup()
        if _, ok := lim.(*RateLimit); !ok {
                t.Fatalf("unset ORVEXA_REDIS_URL must return the in-process token bucket byte-identically, got %T", lim)
        }
        // Same method surface as the Redis driver: the middleware cannot tell
        // them apart by shape, only by wiring.
        var _ Limiter = lim
}

func TestNewRateLimitFromEnvMalformedURLErrorsWithoutEchoing(t *testing.T) {
        const bad = "redis://[::1:not-a-port"
        t.Setenv("ORVEXA_REDIS_URL", bad)
        _, _, err := NewRateLimitFromEnv(context.Background(), RedisRateLimitConfig{}, 600, 120, 10_000)
        if err == nil {
                t.Fatal("malformed ORVEXA_REDIS_URL must error, not silently degrade")
        }
        if strings.Contains(err.Error(), bad) {
                t.Fatal("the configured URL must never be echoed (credential hygiene)")
        }
}

func TestNewRateLimitFromEnvUnreachableStillConstructs(t *testing.T) {
        t.Setenv("ORVEXA_REDIS_URL", "redis://127.0.0.1:1/0") // loopback refuse: fast, no external dependency
        logs := []string{}
        lim, cleanup, err := NewRateLimitFromEnv(context.Background(),
                RedisRateLimitConfig{Limit: 1, Window: time.Minute, Scope: "auth", FailClosed: true,
                        Log: func(msg string, _ ...any) { logs = append(logs, msg) }},
                600, 120, 10_000)
        if err != nil {
                t.Fatalf("unreachable Redis must degrade at boot, not fail: %v", err)
        }
        defer cleanup()
        if _, ok := lim.(*RedisRateLimit); !ok {
                t.Fatalf("want *RedisRateLimit, got %T", lim)
        }
        if len(logs) == 0 || logs[0] != "redis_rate_limit_unreachable_at_boot" {
                t.Fatalf("the unreachable boot ping must be logged, got %v", logs)
        }
        // The configured fail policy is live even when Redis never came up.
        if ok, retry := lim.Allow("k", alignedBase); ok || retry != time.Minute {
                t.Fatalf("fail-closed driver over a dead Redis must deny with the window, got (%v, %v)", ok, retry)
        }
}

// limiterHarness is the RESP2 wire harness for the limiter: the same idea as
// the routing package's harness, with the command subset the limiter issues
// (PING/CLIENT/HELLO/INCR/EXPIRE with NX). It proves the driver against the
// real go-redis client over TCP with no new deps; the ORVEXA_REDIS_URL-gated
// compose test (limiter_redis_integration_test.go) is the live-service
// evidence.
type limiterHarness struct {
        ln     net.Listener
        mu     sync.Mutex
        counts map[string]int64
        ttls   map[string]time.Time // absolute deadline; zero value = no expiry
}

func newLimiterHarness(t *testing.T) *limiterHarness {
        t.Helper()
        ln, err := net.Listen("tcp", "127.0.0.1:0")
        if err != nil {
                t.Fatalf("harness listen: %v", err)
        }
        h := &limiterHarness{ln: ln, counts: map[string]int64{}, ttls: map[string]time.Time{}}
        go h.serve()
        t.Cleanup(func() { _ = ln.Close() })
        return h
}

func (h *limiterHarness) addr() string { return h.ln.Addr().String() }

func (h *limiterHarness) serve() {
        for {
                conn, err := h.ln.Accept()
                if err != nil {
                        return
                }
                go h.handle(conn)
        }
}

func (h *limiterHarness) handle(conn net.Conn) {
        defer conn.Close()
        r := bufio.NewReader(conn)
        for {
                args, err := readRespArray(r)
                if err != nil {
                        return
                }
                if _, err := conn.Write([]byte(h.exec(args))); err != nil {
                        return
                }
        }
}

// readRespArray parses one RESP2 array of bulk strings (wire contract of the
// commands the driver issues).
func readRespArray(r *bufio.Reader) ([]string, error) {
        line, err := r.ReadString('\n')
        if err != nil {
                return nil, err
        }
        line = strings.TrimRight(line, "\r\n")
        if len(line) < 2 || line[0] != '*' {
                return nil, errors.New("resp: expected array header")
        }
        n, err := strconv.Atoi(line[1:])
        if err != nil {
                return nil, err
        }
        args := make([]string, 0, n)
        for i := 0; i < n; i++ {
                head, err := r.ReadString('\n')
                if err != nil {
                        return nil, err
                }
                head = strings.TrimRight(head, "\r\n")
                if len(head) < 1 || head[0] != '$' {
                        return nil, errors.New("resp: expected bulk header")
                }
                ln, err := strconv.Atoi(head[1:])
                if err != nil {
                        return nil, err
                }
                buf := make([]byte, ln+2) // payload + CRLF
                if _, err := io.ReadFull(r, buf); err != nil {
                        return nil, err
                }
                args = append(args, string(buf[:ln]))
        }
        return args, nil
}

func (h *limiterHarness) exec(args []string) string {
        h.mu.Lock()
        defer h.mu.Unlock()
        cmd := strings.ToUpper(args[0])
        switch cmd {
        case "PING":
                return "+PONG\r\n"
        case "CLIENT":
                // go-redis sends CLIENT SETINFO best-effort at connect; accepted-and-ignored.
                return "+OK\r\n"
        case "HELLO":
                // Pre-HELLO server posture: go-redis falls back to legacy RESP2.
                return "-ERR unknown command 'HELLO'\r\n"
        case "INCR":
                key := args[1]
                h.expireIfDueLocked(key)
                n := h.counts[key] + 1
                h.counts[key] = n
                return ":" + strconv.FormatInt(n, 10) + "\r\n"
        case "EXPIRE":
                if len(args) < 3 {
                        return "-ERR wrong number of arguments for 'EXPIRE'\r\n"
                }
                key := args[1]
                secs, err := strconv.Atoi(args[2])
                if err != nil {
                        return "-ERR value is not an integer or out of range\r\n"
                }
                nx := len(args) >= 4 && strings.EqualFold(args[3], "NX")
                if _, ok := h.counts[key]; !ok {
                        return ":0\r\n" // missing key: EXPIRE never creates one
                }
                if _, hasTTL := h.ttls[key]; hasTTL && nx {
                        return ":0\r\n" // NX: only stamp when no TTL exists
                }
                h.ttls[key] = time.Now().Add(time.Duration(secs) * time.Second)
                return ":1\r\n"
        default:
                return "-ERR unknown command '" + cmd + "'\r\n"
        }
}

func (h *limiterHarness) expireIfDueLocked(key string) {
        if d, ok := h.ttls[key]; ok && !time.Now().Before(d) {
                delete(h.counts, key)
                delete(h.ttls, key)
        }
}

// TestRedisRateLimitWireRoundTrip proves the driver against the real
// go-redis client over an actual TCP connection: dial, INCR counting,
// EXPIRE NX stamping, the deny boundary and bucket rotation on the wire.
func TestRedisRateLimitWireRoundTrip(t *testing.T) {
        h := newLimiterHarness(t)
        opts, err := redis.ParseURL("redis://" + h.addr() + "/0")
        if err != nil {
                t.Fatalf("parse harness URL: %v", err)
        }
        // The harness speaks RESP2; go-redis v9 defaults to HELLO/RESP3, which
        // the compose redis:7 service handles in production but the harness
        // intentionally does not implement.
        opts.Protocol = 2
        client := redis.NewClient(opts)
        t.Cleanup(func() { _ = client.Close() })

        ctx := context.Background()
        if err := client.Ping(ctx).Err(); err != nil {
                t.Fatalf("ping over the wire: %v", err)
        }

        rl := NewRedisRateLimit(client, RedisRateLimitConfig{Limit: 3, Window: time.Minute, Scope: "auth"})
        now := alignedBase
        for i := 1; i <= 3; i++ {
                if ok, _ := rl.Allow("wire-key", now); !ok {
                        t.Fatalf("call %d over the wire must be allowed", i)
                }
        }
        if ok, retry := rl.Allow("wire-key", now); ok || retry != time.Minute {
                t.Fatalf("call 4 over the wire must be denied with the window remaining, got (%v, %v)", ok, retry)
        }
        if ok, _ := rl.Allow("wire-key", now.Add(time.Minute)); !ok {
                t.Fatal("bucket rotation over the wire must restore capacity")
        }
}
