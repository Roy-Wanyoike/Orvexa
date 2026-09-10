package identity

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dbRoleResolver resolves role bindings from storage (migration
// 0012_rbac.sql) behind a short-TTL, bounded cache. Authorization changes
// (binding granted/revoked) take effect within the TTL without redeploying.
//
// The query joins role_bindings (tenant, principal, role, active) to the
// capability_catalog (role → capability), so a binding revocation removes
// capabilities even if the token is still cryptographically valid — tokens
// assert identity; the database owns authorization.
type dbRoleResolver struct {
	pool       *pgxpool.Pool
	ttl        time.Duration
	maxEntries int
	now        func() time.Time

	mu    sync.Mutex
	cache map[string]bindingCacheEntry
}

type bindingCacheEntry struct {
	binding  Binding
	resolved time.Time
}

// NewDBRoleResolver returns the storage-backed RoleResolver. ttl defaults to
// 30s; maxEntries defaults to 4096 tenants×subjects (each entry is two small
// string slices — worst case is bounded and tiny).
func NewDBRoleResolver(pool *pgxpool.Pool, ttl time.Duration, maxEntries int) RoleResolver {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &dbRoleResolver{
		pool:       pool,
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
		cache:      map[string]bindingCacheEntry{},
	}
}

// Resolve returns the distinct role set and the union capability set for
// (tenant, subject). Unknown pairs resolve to an EMPTY binding — that is the
// security-critical default (no binding, no capabilities → 403 downstream).
func (r *dbRoleResolver) Resolve(ctx context.Context, tenantID, subject string) (Binding, error) {
	key := tenantID + "|" + subject
	r.mu.Lock()
	if e, ok := r.cache[key]; ok && r.now().Sub(e.resolved) <= r.ttl {
		r.mu.Unlock()
		return e.binding, nil
	}
	r.mu.Unlock()

	rows, err := r.pool.Query(ctx, `
                SELECT DISTINCT rb.role, cc.capability
                FROM role_bindings rb
                JOIN capability_catalog cc ON cc.role = rb.role
                WHERE rb.tenant_id = $1 AND rb.principal = $2 AND rb.status = 'active'
        `, tenantID, subject)
	if err != nil {
		return Binding{}, err
	}
	defer rows.Close()

	roleSet := map[string]bool{}
	capSet := map[string]bool{}
	for rows.Next() {
		var role, capability string
		if err := rows.Scan(&role, &capability); err != nil {
			return Binding{}, err
		}
		roleSet[role] = true
		capSet[capability] = true
	}
	if err := rows.Err(); err != nil {
		return Binding{}, err
	}

	binding := Binding{Roles: orderedRoles(roleSet), Capabilities: capabilitiesFromSet(capSet)}
	r.mu.Lock()
	if len(r.cache) >= r.maxEntries {
		r.evictLocked()
	}
	r.cache[key] = bindingCacheEntry{binding: binding, resolved: r.now()}
	r.mu.Unlock()
	return binding, nil
}

// evictLocked drops expired entries, falling back to a full reset when the
// bound is still exceeded (bounded memory under adversarial key churn).
func (r *dbRoleResolver) evictLocked() {
	now := r.now()
	for k, e := range r.cache {
		if now.Sub(e.resolved) > r.ttl {
			delete(r.cache, k)
		}
	}
	if len(r.cache) >= r.maxEntries {
		r.cache = map[string]bindingCacheEntry{}
	}
}
