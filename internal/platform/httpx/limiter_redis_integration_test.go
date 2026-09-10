//go:build integration

// Redis rate-limiter integration suite (issue #37, [O-28]). Runs ONLY
// against a real Redis — the compose path:
//
//	docker compose -f docker-compose.dev.yml up -d redis
//	ORVEXA_REDIS_URL=redis://localhost:6379/0 go test -race -tags=integration ./internal/platform/httpx/
//
// It skips cleanly (no failure) whenever ORVEXA_REDIS_URL is unset, so the
// default `go test ./...` and CI matrices are unaffected. The in-package
// fake + RESP2 harness in limiter_redis_test.go cover every unit-testable
// path (including both fail policies, which a healthy Redis cannot trigger);
// this file proves the global, cross-node semantics on the live service.
package httpx

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestRedisRateLimitComposeIntegration proves the property that motivates
// the driver: the limit is GLOBAL, not per-instance. Two independent Redis
// clients (simulating two orvexa-api binaries behind a load balancer) share
// one bucket: A consumes part of the budget, B is denied once the shared
// count crosses the limit, and capacity is restored in the next window for
// both. Uses a unique scope per run so it never collides with other suites.
func TestRedisRateLimitComposeIntegration(t *testing.T) {
	url := os.Getenv("ORVEXA_REDIS_URL")
	if url == "" {
		t.Skip("ORVEXA_REDIS_URL not set — skipping compose Redis integration")
	}

	newClient := func() *redis.Client {
		t.Helper()
		opts, err := redis.ParseURL(url)
		if err != nil {
			t.Fatalf("parse ORVEXA_REDIS_URL: %v", err)
		}
		c := redis.NewClient(opts)
		t.Cleanup(func() { _ = c.Close() })
		if err := c.Ping(context.Background()).Err(); err != nil {
			t.Fatalf("ORVEXA_REDIS_URL unreachable: %v", err)
		}
		return c
	}
	nodeA, nodeB := newClient(), newClient() // "two binaries", same Redis

	const limit = 5
	window := 2 * time.Second
	scope := "it-" + uuid.NewString() // per-run namespace: never collides
	cfg := func() RedisRateLimitConfig {
		return RedisRateLimitConfig{Limit: limit, Window: window, Scope: scope}
	}
	limA := NewRedisRateLimit(nodeA, cfg())
	limB := NewRedisRateLimit(nodeB, cfg())

	// nodeA spends 3, nodeB spends the remaining 2 — shared across "nodes".
	allowedA, allowedB := 0, 0
	for i := 0; i < 3; i++ {
		if ok, _ := limA.Allow("tenant-shared", time.Now()); ok {
			allowedA++
		}
	}
	for i := 0; i < 3; i++ { // limit is 5: B gets 2, its third call is denied
		if ok, retry := limB.Allow("tenant-shared", time.Now()); ok {
			allowedB++
		} else if retry <= 0 {
			t.Fatal("deny must carry a positive retry (time until the bucket rotates)")
		}
	}
	if allowedA != 3 || allowedB != 2 {
		t.Fatalf("global budget must be shared across nodes: A allowed %d/3, B allowed %d/3 (want 3 and 2)", allowedA, allowedB)
	}

	// The shared bucket key carries the server-side TTL of the window.
	bucket := time.Now().UnixNano() / int64(window)
	rkey := "orvexa:rl:v1:" + scope + ":tenant-shared:" + itoa(bucket)
	ttl, err := nodeA.TTL(context.Background(), rkey).Result()
	if err != nil || ttl <= 0 || ttl > window {
		t.Fatalf("shared bucket TTL = %v err = %v, want 0 < TTL <= %v", ttl, err, window)
	}

	// Next window: capacity restored for BOTH nodes.
	time.Sleep(window + 150*time.Millisecond) // strictly past the bucket boundary
	for i := 0; i < limit; i++ {
		if ok, _ := limA.Allow("tenant-shared", time.Now()); !ok {
			t.Fatalf("call %d of the fresh window must be allowed on node A", i+1)
		}
	}
	if ok, _ := limB.Allow("tenant-shared", time.Now()); ok {
		t.Fatal("the fresh window's budget must again be shared: node B must be denied once node A drains it")
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
