//go:build integration

// Redis presence-cache integration suite (issue #37, [O-28]). Runs ONLY
// against a real Redis — the compose path:
//
//	docker compose -f docker-compose.dev.yml up -d redis
//	ORVEXA_REDIS_URL=redis://localhost:6379/0 go test -race -tags=integration ./internal/routing/
//
// It skips cleanly (no failure) whenever ORVEXA_REDIS_URL is unset, so the
// default `go test ./...` and CI matrices are unaffected. The in-package
// fake + RESP2 harness in presence_redis_test.go cover every unit-testable
// path; this file is the live-wire evidence the acceptance criteria ask for.
package routing

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestRedisPresenceComposeIntegration runs the full driver lifecycle against
// the real redis:7 service from docker-compose.dev.yml: miss → DB load →
// write-back with TTL, cache hit without a DB load, explicit invalidation
// forcing a reload, and server-side TTL expiry forcing a reload.
func TestRedisPresenceComposeIntegration(t *testing.T) {
	url := os.Getenv("ORVEXA_REDIS_URL")
	if url == "" {
		t.Skip("ORVEXA_REDIS_URL not set — skipping compose Redis integration")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse ORVEXA_REDIS_URL: %v", err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ORVEXA_REDIS_URL unreachable: %v", err)
	}

	// Unique tenant per run: never collides with other suites or runs.
	tenant := "it-" + uuid.NewString()
	loads := 0
	cache := newRedisPresenceCache(client, func(context.Context, string) (map[string]bool, error) {
		loads++
		return map[string]bool{"agent-" + strconv.Itoa(loads): true}, nil
	}, time.Second, func(msg string, args ...any) { t.Logf("%s %v", msg, args) })

	set, err := cache.AvailableAgents(ctx, tenant)
	if err != nil || loads != 1 || !set["agent-1"] {
		t.Fatalf("miss must load: loads=%d set=%v err=%v", loads, set, err)
	}
	if _, err := cache.AvailableAgents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("second read must hit Redis, loads = %d", loads)
	}
	ttl, err := client.TTL(ctx, presenceCacheKey(tenant)).Result()
	if err != nil || ttl <= 0 {
		t.Fatalf("real Redis TTL after write-back = %v err = %v", ttl, err)
	}

	cache.Invalidate(tenant)
	set, err = cache.AvailableAgents(ctx, tenant)
	if err != nil || loads != 2 || !set["agent-2"] {
		t.Fatalf("invalidate must force reload: loads=%d set=%v err=%v", loads, set, err)
	}

	time.Sleep(1100 * time.Millisecond) // TTL is 1s — the key must expire server-side
	if _, err := cache.AvailableAgents(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if loads != 3 {
		t.Fatalf("expired key must reload, loads = %d", loads)
	}
}
