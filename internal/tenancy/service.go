// Package tenancy owns the control-plane primitives every request passes
// through: API-key authentication and tenant resolution. The tenant ALWAYS
// comes from the authenticated key — never from a request body or header.
package tenancy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// Principal is the authenticated caller identity attached to every request.
type Principal struct {
	APIKeyID string
	TenantID string
	Scopes   []string
}

// HasScope reports whether the principal may perform an action class.
func (p *Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == "*" || s == scope {
			return true
		}
	}
	return false
}

type ctxKey int

const ctxPrincipal ctxKey = 200

// PrincipalFrom extracts the authenticated principal from context.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(ctxPrincipal).(*Principal)
	return p, ok
}

// WithPrincipal stores the principal on the request context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxPrincipal, p)
}

// HashKey is the only representation of a raw key that ever touches storage.
func HashKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// NewRawKey returns a fresh API key with a recognizable prefix.
func NewRawKey() string {
	return "orvx_" + randomHex(24)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b)
}

// Service authenticates API keys against storage.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Authenticate resolves a raw key to its principal. Lookup is by hash; the
// SHA-256 preimage resistance makes index-timing attacks irrelevant, and the
// response never discloses whether a key was expired, revoked or unknown.
func (s *Service) Authenticate(ctx context.Context, rawKey string) (*Principal, error) {
	if strings.TrimSpace(rawKey) == "" {
		return nil, apperrors.Unauth("auth.missing_key", "missing API key")
	}
	row := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, scopes
		FROM api_keys
		WHERE key_hash = $1 AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())
	`, HashKey(rawKey))

	var p Principal
	var scopes []string
	if err := row.Scan(&p.APIKeyID, &p.TenantID, &scopes); err != nil {
		if err == pgx.ErrNoRows {
			return nil, apperrors.Unauth("auth.invalid_key", "invalid API key")
		}
		return nil, apperrors.Internal("auth.lookup_failed", "authentication unavailable").WithCause(err)
	}
	p.Scopes = scopes
	return &p, nil
}

// TouchKey records last-used time (best-effort; never blocks the request).
func (s *Service) TouchKey(ctx context.Context, keyID string) {
	_, _ = s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, keyID)
}

// ConstantTimeEqual is exported for tests and future token comparisons.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ErrAuthRequired is the sentinel for transports needing explicit auth
// errors (e.g. realtime upgrades).
var ErrAuthRequired = apperrors.Unauth("auth.missing_key", "missing API key")
