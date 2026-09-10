// Redis-backed presence cache — issue #37 [O-28] (additive driver file;
// presence.go is untouched and remains the default when ORVEXA_REDIS_URL is
// unset, byte-identical to the pre-#37 behavior).
//
// Semantics, documented honestly and enforced by tests:
//
//   - DB is the source of truth. agent_presence is read through the same
//     query PresenceStore uses; nothing here ever writes presence.
//   - Redis is a shared TTL snapshot cache. A hit returns the stored
//     snapshot; a miss (missing key, expired key, Redis error, undecodable
//     payload) falls back to the DB read and repopulates Redis best-effort.
//   - Redis failures NEVER fail a read: dial errors, command errors and
//     decode errors are logged (never keys, URLs or credentials) and treated
//     as a miss. Only DB errors propagate, exactly like the in-process store.
//   - Invalidation is explicit and shared: Invalidate deletes the tenant's
//     cache key so every node sharing Redis sees the drop. A failed DEL is
//     logged; the TTL then bounds staleness, mirroring the in-process
//     store's TTL bound.
//   - Wire format: one STRING key per tenant, "orvexa:presence:v1:<tenantID>",
//     holding a JSON array of available agent IDs (sorted, deterministic).
//     An empty array caches a known-empty set (negative caching) and is
//     distinct from a missing key (a miss).
//
// Env: ORVEXA_REDIS_URL (e.g. redis://localhost:6379/0). NewPresenceCacheFromEnv
// returns the existing in-process PresenceStore when it is unset.
package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// PresenceCache is the swap-in surface shared by the in-process PresenceStore
// and the Redis-backed driver below: the PresenceReader the routing engine
// consumes, plus the explicit invalidation hook presence writers call.
type PresenceCache interface {
	PresenceReader
	// Invalidate drops the cached snapshot for a tenant.
	Invalidate(tenantID string)
}

// Compile-time swap contract: both drivers implement the same surface, so
// wiring can switch between them (env-driven) without touching consumers.
var (
	_ PresenceCache = (*PresenceStore)(nil)
	_ PresenceCache = (*RedisPresenceCache)(nil)
)

// presenceLoader abstracts the DB-truth read (the exact query from
// presence.go). It exists so tests can exercise every cache path without a
// live PostgreSQL.
type presenceLoader func(ctx context.Context, tenantID string) (map[string]bool, error)

const (
	presenceRedisPrefix     = "orvexa:presence:v1:"
	presenceRedisDefaultTTL = 2 * time.Second // parity with presence.go's default
	redisBootTimeout        = 2 * time.Second // startup Ping bound
	redisInvalidateTimeout  = 2 * time.Second // Invalidate is ctx-less at the interface
)

// RedisPresenceCache implements PresenceCache with agent_presence (DB) as the
// source of truth and Redis as a shared TTL snapshot cache for multi-binary
// deployments (O-24): every node sharing Redis shares both the snapshot and
// the invalidation, and the DB is never bypassed on a miss.
//
// The zero-value contract: safe for concurrent use (the client is
// goroutine-safe; the struct is immutable after construction).
type RedisPresenceCache struct {
	client redis.UniversalClient
	load   presenceLoader
	ttl    time.Duration
	log    func(msg string, args ...any) // never nil after construction
}

// NewRedisPresenceCache builds the Redis-backed driver over a pgxpool. The
// caller retains ownership of client (close it via the cleanup returned by
// NewPresenceCacheFromEnv, or their own wiring). ttl <= 0 → 2s, matching
// presence.go's default.
func NewRedisPresenceCache(client redis.UniversalClient, pool *pgxpool.Pool, ttl time.Duration, log func(msg string, args ...any)) *RedisPresenceCache {
	return newRedisPresenceCache(client, dbPresenceLoader(pool), ttl, log)
}

func newRedisPresenceCache(client redis.UniversalClient, load presenceLoader, ttl time.Duration, log func(msg string, args ...any)) *RedisPresenceCache {
	if ttl <= 0 {
		ttl = presenceRedisDefaultTTL
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return &RedisPresenceCache{client: client, load: load, ttl: ttl, log: log}
}

// NewPresenceCacheFromEnv is the env-gated swap point.
//
//   - ORVEXA_REDIS_URL unset → (NewPresenceStore(pool, ttl), noop cleanup,
//     nil): the exact in-process driver, byte-identical to the pre-#37
//     default path.
//   - Set but malformed → error. Operator intent is explicit, so
//     misconfiguration is never silently swallowed; the configured value is
//     NOT echoed (credential hygiene).
//   - Set but unreachable → the Redis driver is still returned; every
//     operation then degrades to plain DB reads (logged) until Redis
//     recovers. Presence is a cache, so a dead Redis must never take the
//     routing engine down.
func NewPresenceCacheFromEnv(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration, log func(msg string, args ...any)) (PresenceCache, func(), error) {
	url := os.Getenv("ORVEXA_REDIS_URL")
	if url == "" {
		return NewPresenceStore(pool, ttl), func() {}, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, nil, errors.New("ORVEXA_REDIS_URL is not a valid redis URL (expected redis://[user:pass@]host:port[/db]); the configured value is not echoed")
	}
	client := redis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, redisBootTimeout)
	pingErr := client.Ping(pingCtx).Err()
	cancel()
	if pingErr != nil && log != nil {
		// Dial errors carry host:port only — never credentials.
		log("redis_presence_cache_unreachable_at_boot", "err", pingErr)
	}
	cache := NewRedisPresenceCache(client, pool, ttl, log)
	return cache, func() { _ = client.Close() }, nil
}

// AvailableAgents implements PresenceReader. Cache hit → snapshot; anything
// else → DB truth, repopulated into Redis best-effort. Redis errors are
// logged and swallowed; DB errors propagate (parity with PresenceStore).
func (c *RedisPresenceCache) AvailableAgents(ctx context.Context, tenantID string) (map[string]bool, error) {
	key := presenceCacheKey(tenantID)
	if payload, err := c.client.Get(ctx, key).Result(); err == nil {
		if set, ok := decodePresenceSnapshot(payload); ok {
			return set, nil
		}
		c.log("redis_presence_cache_error", "op", "decode")
	} else if !errors.Is(err, redis.Nil) {
		c.log("redis_presence_cache_error", "op", "get", "err", err)
	}

	set, err := c.load(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if payload, err := json.Marshal(presenceSnapshot(set)); err == nil {
		if err := c.client.Set(ctx, key, payload, c.ttl).Err(); err != nil {
			c.log("redis_presence_cache_error", "op", "set", "err", err)
		}
	} else {
		c.log("redis_presence_cache_error", "op", "encode", "err", err)
	}
	return set, nil
}

// Invalidate implements the explicit invalidation hook. It deletes the
// tenant's shared cache key (bounded by redisInvalidateTimeout since the
// interface signature is ctx-less) and is best-effort: a failure is logged
// and the TTL bounds staleness.
func (c *RedisPresenceCache) Invalidate(tenantID string) {
	ctx, cancel := context.WithTimeout(context.Background(), redisInvalidateTimeout)
	defer cancel()
	if err := c.client.Del(ctx, presenceCacheKey(tenantID)).Err(); err != nil {
		c.log("redis_presence_cache_invalidate_failed", "err", err)
	}
}

func presenceCacheKey(tenantID string) string {
	return presenceRedisPrefix + tenantID
}

// presenceSnapshot projects the available set into its deterministic wire
// form: sorted agent IDs.
func presenceSnapshot(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id, available := range set {
		if available {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// decodePresenceSnapshot parses the wire form; ok=false → caller treats the
// payload as a miss.
func decodePresenceSnapshot(payload string) (map[string]bool, bool) {
	var ids []string
	if err := json.Unmarshal([]byte(payload), &ids); err != nil {
		return nil, false
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, true
}

// dbPresenceLoader wraps the DB-truth read — the same query presence.go runs
// (duplicated here because presence.go must stay byte-identical; the two are
// asserted behaviorally equivalent by the swap tests in the integration
// path and by review).
func dbPresenceLoader(pool *pgxpool.Pool) presenceLoader {
	return func(ctx context.Context, tenantID string) (map[string]bool, error) {
		rows, err := pool.Query(ctx, `
			SELECT agent_id FROM agent_presence
			WHERE tenant_id = $1 AND status = 'available'`, tenantID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		available := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			available[id] = true
		}
		return available, rows.Err()
	}
}
