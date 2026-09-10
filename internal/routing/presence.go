// Package routing — presence store: DB is the source of truth; the in-process
// cache serves the hot path with a short TTL so routing decisions never wait
// on a presence write from another node.
package routing

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PresenceStore implements PresenceReader backed by agent_presence (DB truth)
// with an in-process TTL cache for the hot path.
type PresenceStore struct {
	pool *pgxpool.Pool
	ttl  time.Duration

	mu      sync.Mutex
	cache   map[string]cacheEntry // tenantID → snapshot
}

type cacheEntry struct {
	available map[string]bool
	expires   time.Time
}

func NewPresenceStore(pool *pgxpool.Pool, ttl time.Duration) *PresenceStore {
	if ttl <= 0 {
		ttl = 2 * time.Second
	}
	return &PresenceStore{pool: pool, ttl: ttl, cache: map[string]cacheEntry{}}
}

// AvailableAgents returns the set of agents currently marked available.
func (p *PresenceStore) AvailableAgents(ctx context.Context, tenantID string) (map[string]bool, error) {
	p.mu.Lock()
	if e, ok := p.cache[tenantID]; ok && time.Now().Before(e.expires) {
		p.mu.Unlock()
		return e.available, nil
	}
	p.mu.Unlock()

	rows, err := p.pool.Query(ctx, `
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
	p.mu.Lock()
	p.cache[tenantID] = cacheEntry{available: available, expires: time.Now().Add(p.ttl)}
	p.mu.Unlock()
	return available, nil
}

// Invalidate drops the cached snapshot for a tenant (called on presence writes).
func (p *PresenceStore) Invalidate(tenantID string) {
	p.mu.Lock()
	delete(p.cache, tenantID)
	p.mu.Unlock()
}
