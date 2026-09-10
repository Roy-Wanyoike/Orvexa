package routing

import (
	"bufio"
	"context"
	"encoding/json"
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

// fakeRedis is a minimal in-process implementation of the go-redis
// UniversalClient surface covering exactly the commands the presence driver
// issues (GET/SET/DEL/CLOSE). No new deps, no miniredis: the embedded nil
// interface satisfies UniversalClient, so any command the driver should
// never issue would panic loudly in tests instead of passing silently.
type fakeRedis struct {
	redis.UniversalClient

	mu      sync.Mutex
	clock   func() time.Time
	strs    map[string]string
	expires map[string]time.Time // absolute deadline; zero value = no expiry
	failure error                // non-nil → every command fails with it
	calls   map[string]int
}

var _ redis.UniversalClient = (*fakeRedis)(nil)

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		clock:   time.Now,
		strs:    map[string]string{},
		expires: map[string]time.Time{},
		calls:   map[string]int{},
	}
}

func (f *fakeRedis) count(op string) { f.calls[op]++ }

// expireIfDue lazily enforces the stored deadline (real Redis deletes
// expired keys on access).
func (f *fakeRedis) expireIfDue(key string) {
	if d, ok := f.expires[key]; ok && !f.clock().Before(d) {
		delete(f.strs, key)
		delete(f.expires, key)
	}
}

func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("get")
	if f.failure != nil {
		return redis.NewStringResult("", f.failure)
	}
	f.expireIfDue(key)
	if v, ok := f.strs[key]; ok {
		return redis.NewStringResult(v, nil)
	}
	return redis.NewStringResult("", redis.Nil)
}

func (f *fakeRedis) Set(_ context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("set")
	if f.failure != nil {
		return redis.NewStatusResult("", f.failure)
	}
	s, ok := value.(string)
	if !ok {
		if b, isBytes := value.([]byte); isBytes {
			s, ok = string(b), true // go-redis encodes []byte as a bulk string
		}
	}
	if !ok {
		return redis.NewStatusResult("", errors.New("fake: only string/[]byte values supported"))
	}
	f.strs[key] = s
	delete(f.expires, key) // SET clears the TTL unless KEEPTTL — real-Redis semantics
	if expiration > 0 {
		f.expires[key] = f.clock().Add(expiration)
	}
	return redis.NewStatusResult("OK", nil)
}

func (f *fakeRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("del")
	if f.failure != nil {
		return redis.NewIntResult(0, f.failure)
	}
	n := int64(0)
	for _, key := range keys {
		if _, ok := f.strs[key]; ok {
			n++
		}
		delete(f.strs, key)
		delete(f.expires, key)
	}
	return redis.NewIntResult(n, nil)
}

func (f *fakeRedis) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["close"]++
	return nil
}

func TestRedisPresenceCacheHitSkipsDB(t *testing.T) {
	fr := newFakeRedis()
	ctx := context.Background()
	payload, err := json.Marshal(presenceSnapshot(map[string]bool{"a1": true, "a2": true}))
	if err != nil {
		t.Fatal(err)
	}
	if err := fr.Set(ctx, presenceCacheKey("t1"), string(payload), time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	dbErr := errors.New("DB must not be read on a cache hit")
	cache := newRedisPresenceCache(fr, func(context.Context, string) (map[string]bool, error) {
		return nil, dbErr
	}, time.Minute, nil)

	set, err := cache.AvailableAgents(ctx, "t1")
	if err != nil {
		t.Fatalf("cache hit must not fail: %v", err)
	}
	if len(set) != 2 || !set["a1"] || !set["a2"] {
		t.Fatalf("unexpected set from cache: %v", set)
	}
}

func TestRedisPresenceCacheMissLoadsFromDBAndWritesBack(t *testing.T) {
	fr := newFakeRedis()
	ctx := context.Background()
	base := time.Now()
	fr.clock = func() time.Time { return base } // deterministic TTL assertions
	loads := 0
	cache := newRedisPresenceCache(fr, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{"a7": true}, nil
	}, time.Minute, nil)

	set, err := cache.AvailableAgents(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if loads != 1 || len(set) != 1 || !set["a7"] {
		t.Fatalf("miss must load from DB: loads=%d set=%v", loads, set)
	}
	key := presenceCacheKey("t1")
	if got := fr.strs[key]; got != `["a7"]` {
		t.Fatalf("write-back payload = %q, want [\"a7\"]", got)
	}
	if d, ok := fr.expires[key]; !ok || !d.Equal(base.Add(time.Minute)) {
		t.Fatalf("write-back TTL deadline = %v (ok=%v), want %v", d, ok, base.Add(time.Minute))
	}

	// Second read must be served from the cache without another DB load.
	if _, err := cache.AvailableAgents(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("second read must hit the cache, loads = %d", loads)
	}
}

func TestRedisPresenceCacheInvalidateForcesReload(t *testing.T) {
	fr := newFakeRedis()
	ctx := context.Background()
	loads := 0
	cache := newRedisPresenceCache(fr, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{"a-" + strconv.Itoa(loads): true}, nil
	}, time.Minute, nil)

	if _, err := cache.AvailableAgents(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate("t1")
	if _, ok := fr.strs[presenceCacheKey("t1")]; ok {
		t.Fatal("Invalidate must delete the shared cache key")
	}
	set, err := cache.AvailableAgents(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if loads != 2 || !set["a-2"] {
		t.Fatalf("read after invalidate must reload: loads=%d set=%v", loads, set)
	}
}

func TestRedisPresenceCorruptSnapshotIsTreatedAsMiss(t *testing.T) {
	fr := newFakeRedis()
	ctx := context.Background()
	if err := fr.Set(ctx, presenceCacheKey("t1"), "{not-json", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	loads := 0
	cache := newRedisPresenceCache(fr, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{"recovered": true}, nil
	}, time.Minute, nil)

	set, err := cache.AvailableAgents(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if loads != 1 || !set["recovered"] {
		t.Fatalf("corrupt payload must degrade to a DB read: loads=%d set=%v", loads, set)
	}
	if got := fr.strs[presenceCacheKey("t1")]; got != `["recovered"]` {
		t.Fatalf("corrupt payload must be overwritten, got %q", got)
	}
}

func TestRedisPresenceRedisFailureFallsBackToDB(t *testing.T) {
	fr := newFakeRedis()
	fr.failure = errors.New("redis down")
	ctx := context.Background()
	loads := 0
	cache := newRedisPresenceCache(fr, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{"a1": true}, nil
	}, time.Minute, nil)

	for i := 1; i <= 2; i++ {
		set, err := cache.AvailableAgents(ctx, "t1")
		if err != nil {
			t.Fatalf("Redis outage must never fail a presence read: %v", err)
		}
		if loads != i || !set["a1"] {
			t.Fatalf("read %d must fall back to DB: loads=%d set=%v", i, loads, set)
		}
	}
	if len(fr.strs) != 0 {
		t.Fatalf("failed SET must not store anything, got %v", fr.strs)
	}
}

func TestRedisPresenceEmptySetIsCachedNegatively(t *testing.T) {
	fr := newFakeRedis()
	ctx := context.Background()
	loads := 0
	cache := newRedisPresenceCache(fr, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{}, nil
	}, time.Minute, nil)

	set, err := cache.AvailableAgents(ctx, "t1")
	if err != nil || len(set) != 0 {
		t.Fatalf("first read: set=%v err=%v", set, err)
	}
	if got := fr.strs[presenceCacheKey("t1")]; got != "[]" {
		t.Fatalf("known-empty set must be cached as [], got %q", got)
	}
	if _, err := cache.AvailableAgents(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("known-empty snapshot must serve subsequent reads, loads = %d", loads)
	}
}

func TestRedisPresenceSnapshotWireFormIsSortedAndRoundTrips(t *testing.T) {
	payload, err := json.Marshal(presenceSnapshot(map[string]bool{"b": true, "a": true, "c": true}))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `["a","b","c"]` {
		t.Fatalf("wire form must be sorted and deterministic, got %s", payload)
	}
	decoded, ok := decodePresenceSnapshot(string(payload))
	if !ok || len(decoded) != 3 || !decoded["a"] || !decoded["b"] || !decoded["c"] {
		t.Fatalf("round trip failed: %v ok=%v", decoded, ok)
	}
	if _, ok := decodePresenceSnapshot("{oops"); ok {
		t.Fatal("corrupt payload must not decode")
	}
}

func TestRedisPresenceCacheTTLDefaultMatchesInProcessStore(t *testing.T) {
	cache := newRedisPresenceCache(newFakeRedis(), nil, 0, nil)
	store := NewPresenceStore(nil, 0)
	if cache.ttl != store.ttl {
		t.Fatalf("Redis default TTL %v must match the in-process default %v", cache.ttl, store.ttl)
	}
}

func TestRedisPresenceCacheKeysAreTenantScoped(t *testing.T) {
	if got := presenceCacheKey("tenant-1"); got != "orvexa:presence:v1:tenant-1" {
		t.Fatalf("unexpected key format: %q", got)
	}
	fr := newFakeRedis()
	ctx := context.Background()
	cache := newRedisPresenceCache(fr, func(_ context.Context, tenant string) (map[string]bool, error) {
		return map[string]bool{tenant: true}, nil
	}, time.Minute, nil)
	if _, err := cache.AvailableAgents(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.AvailableAgents(ctx, "t2"); err != nil {
		t.Fatal(err)
	}
	if fr.strs[presenceCacheKey("t1")] != `["t1"]` || fr.strs[presenceCacheKey("t2")] != `["t2"]` {
		t.Fatalf("tenant snapshots must not bleed: %v", fr.strs)
	}
}

func TestNewPresenceCacheFromEnvUnsetReturnsInProcessStore(t *testing.T) {
	t.Setenv("ORVEXA_REDIS_URL", "")
	cache, cleanup, err := NewPresenceCacheFromEnv(context.Background(), nil, time.Second, nil)
	if err != nil {
		t.Fatalf("unset env must never error: %v", err)
	}
	defer cleanup()
	if _, ok := cache.(*PresenceStore); !ok {
		t.Fatalf("unset ORVEXA_REDIS_URL must return the in-process store byte-identically, got %T", cache)
	}
}

func TestNewPresenceCacheFromEnvMalformedURLErrorsWithoutEchoing(t *testing.T) {
	const bad = "redis://[::1:not-a-port"
	t.Setenv("ORVEXA_REDIS_URL", bad)
	_, _, err := NewPresenceCacheFromEnv(context.Background(), nil, time.Second, nil)
	if err == nil {
		t.Fatal("malformed ORVEXA_REDIS_URL must error, not silently degrade")
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatal("the configured URL must never be echoed (credential hygiene)")
	}
}

func TestNewPresenceCacheFromEnvUnreachableRedisStillConstructs(t *testing.T) {
	t.Setenv("ORVEXA_REDIS_URL", "redis://127.0.0.1:1/0") // loopback refuse: fast, no external dependency
	cache, cleanup, err := NewPresenceCacheFromEnv(context.Background(), nil, time.Second, func(string, ...any) {})
	if err != nil {
		t.Fatalf("unreachable Redis must degrade at boot, not fail: %v", err)
	}
	defer cleanup()
	if _, ok := cache.(*RedisPresenceCache); !ok {
		t.Fatalf("want *RedisPresenceCache, got %T", cache)
	}
}

// respHarness is a minimal RESP2 server implementing exactly the command
// subset the drivers issue (PING/GET/SET with EX|PX/DEL/TTL; the limiter
// tests add INCR/EXPIRE NX). It lets the default `go test` suite prove the
// driver↔go-redis wire contract — dial, serialization, the redis.Nil miss
// contract, SET clearing TTLs — against the real go-redis client with no new
// deps and no miniredis. The ORVEXA_REDIS_URL-gated tests against the compose
// redis:7 service (presence_redis_integration_test.go, -tags=integration)
// remain the authoritative integration evidence.
type respHarness struct {
	ln      net.Listener
	mu      sync.Mutex
	strs    map[string]string
	expires map[string]time.Time
}

func newRespHarness(t *testing.T) *respHarness {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resp harness listen: %v", err)
	}
	h := &respHarness{ln: ln, strs: map[string]string{}, expires: map[string]time.Time{}}
	go h.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return h
}

func (h *respHarness) addr() string { return h.ln.Addr().String() }

func (h *respHarness) serve() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		go h.handle(conn)
	}
}

func (h *respHarness) handle(conn net.Conn) {
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

// readRespArray parses one RESP2 array of bulk strings.
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

func (h *respHarness) exec(args []string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	cmd := strings.ToUpper(args[0])
	switch cmd {
	case "PING":
		return "+PONG\r\n"
	case "CLIENT":
		// go-redis sends CLIENT SETINFO best-effort at connect; handshake
		// extras are accepted-and-ignored (real Redis answers CLIENT +OK).
		return "+OK\r\n"
	case "HELLO":
		// Answer like a pre-HELLO Redis server: go-redis's init handshake
		// treats a redis-error reply as "server does not support HELLO" and
		// falls back to legacy RESP2 (AUTH/SELECT when configured).
		return "-ERR unknown command 'HELLO'\r\n"
	case "GET":
		key := args[1]
		h.expireIfDueLocked(key)
		if v, ok := h.strs[key]; ok {
			return "$" + strconv.Itoa(len(v)) + "\r\n" + v + "\r\n"
		}
		return "$-1\r\n"
	case "SET":
		key, val := args[1], args[2]
		h.strs[key] = val
		delete(h.expires, key)
		for i := 3; i+1 < len(args); i += 2 {
			switch strings.ToUpper(args[i]) {
			case "EX":
				if secs, err := strconv.Atoi(args[i+1]); err == nil && secs > 0 {
					h.expires[key] = time.Now().Add(time.Duration(secs) * time.Second)
				}
			case "PX":
				if ms, err := strconv.Atoi(args[i+1]); err == nil && ms > 0 {
					h.expires[key] = time.Now().Add(time.Duration(ms) * time.Millisecond)
				}
			}
		}
		return "+OK\r\n"
	case "DEL":
		n := 0
		for _, key := range args[1:] {
			if _, ok := h.strs[key]; ok {
				n++
			}
			delete(h.strs, key)
			delete(h.expires, key)
		}
		return ":" + strconv.Itoa(n) + "\r\n"
	case "TTL":
		key := args[1]
		h.expireIfDueLocked(key)
		if _, ok := h.strs[key]; !ok {
			return ":-2\r\n"
		}
		d, ok := h.expires[key]
		if !ok {
			return ":-1\r\n"
		}
		secs := int64(time.Until(d).Seconds())
		return ":" + strconv.FormatInt(secs, 10) + "\r\n"
	default:
		return "-ERR unknown command '" + cmd + "'\r\n"
	}
}

func (h *respHarness) expireIfDueLocked(key string) {
	if d, ok := h.expires[key]; ok && !time.Now().Before(d) {
		delete(h.strs, key)
		delete(h.expires, key)
	}
}

// TestRedisPresenceWireRoundTrip proves the driver against the real go-redis
// client over an actual TCP connection: dial, GET/SET/DEL serialization, the
// redis.Nil miss contract, TTL stamping and invalidation.
func TestRedisPresenceWireRoundTrip(t *testing.T) {
	h := newRespHarness(t)
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

	loads := 0
	cache := newRedisPresenceCache(client, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{"w1": true, "w2": true}, nil
	}, 2*time.Second, nil)

	set, err := cache.AvailableAgents(ctx, "wire-tenant")
	if err != nil {
		t.Fatalf("first read (miss → DB → write-back): %v", err)
	}
	if loads != 1 || len(set) != 2 || !set["w1"] || !set["w2"] {
		t.Fatalf("loads=%d set=%v", loads, set)
	}
	if _, err := cache.AvailableAgents(ctx, "wire-tenant"); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("second read must hit the wire cache, loads = %d", loads)
	}
	ttl, err := client.TTL(ctx, presenceCacheKey("wire-tenant")).Result()
	if err != nil || ttl <= 0 {
		t.Fatalf("TTL after write-back = %v err = %v, want 0 < TTL <= 2s", ttl, err)
	}

	cache.Invalidate("wire-tenant")
	if _, err := cache.AvailableAgents(ctx, "wire-tenant"); err != nil {
		t.Fatal(err)
	}
	if loads != 2 {
		t.Fatalf("read after invalidate must reload, loads = %d", loads)
	}
}
