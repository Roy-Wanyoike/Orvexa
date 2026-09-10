package tenancy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// AuthMiddleware returns a handler that authenticates the request and enforces
// the required scope. Public paths must be mounted outside this middleware.
func AuthMiddleware(svc *Service, requiredScope string, limiter *httpx.RateLimit, log httpx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := bearerOrHeader(r)
			principal, err := svc.Authenticate(r.Context(), key)
			if err != nil {
				httpx.WriteError(w, err)
				return
			}
			if !principal.HasScope(requiredScope) {
				httpx.WriteError(w, apperrors.Forbidden("auth.scope_missing",
					"key lacks required scope "+requiredScope))
				return
			}
			// per-principal rate limiting (memory-bounded token bucket)
			if ok, retry := limiter.Allow(principal.APIKeyID, time.Now()); !ok {
				w.Header().Set("Retry-After", retryCeil(retry))
				httpx.WriteError(w, apperrors.RateLimited("auth.rate_limited",
					"rate limit exceeded; retry later"))
				return
			}
			ctx := WithPrincipal(r.Context(), principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func bearerOrHeader(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func retryCeil(d time.Duration) string {
	s := int(d.Seconds())
	if s < 1 {
		s = 1
	}
	return strconv.Itoa(s)
}

// tenancyCtx attaches the principal (tenant context) to the request context.
func tenancyCtx(ctx context.Context, p *Principal) context.Context {
	return WithPrincipal(ctx, p)
}

// ---- capability RBAC surface (additive, [O-29] issue #38) ----
//
// These additions extend the principal model without altering any existing
// behavior: the API-key authentication path above is unchanged, and the new
// capability helpers are consumed by the OIDC identity plane
// (internal/identity) which composes this middleware additively.

// HasCapability reports whether the principal was granted the capability.
// For API-key principals capabilities arrive as minted key scopes; for OIDC
// principals they are the union of role-binding capability sets.
func (p *Principal) HasCapability(capability string) bool {
	return p.HasScope(capability)
}

// Capabilities returns a defensive copy of the principal's granted
// capability set (empty when none). Callers must not mutate principal state.
func (p *Principal) Capabilities() []string {
	if len(p.Scopes) == 0 {
		return nil
	}
	out := make([]string, len(p.Scopes))
	copy(out, p.Scopes)
	return out
}

// IdentityRef records HOW the caller authenticated — auth method plus the
// underlying identity string (IdP subject for OIDC, key row id for API keys).
// It complements Principal (which carries tenant + granted scopes) and lets
// downstream middleware distinguish the enforcement model in effect.
type IdentityRef struct {
	Method   string // "oidc" | "api-key"
	Subject  string // OIDC `sub` claim or API-key row id
	TenantID string // tenant asserted by the credential
}

// AuthMethodOIDC marks credentials verified by the identity plane.
const AuthMethodOIDC = "oidc"

// AuthMethodAPIKey marks credentials verified by the API-key path.
const AuthMethodAPIKey = "api-key"

const ctxIdentityRef ctxKey = 201

// WithIdentityRef attaches the authentication provenance to the context.
func WithIdentityRef(ctx context.Context, ref IdentityRef) context.Context {
	return context.WithValue(ctx, ctxIdentityRef, ref)
}

// IdentityRefFrom extracts the authentication provenance (ok=false when the
// request carries no resolved identity — e.g. before authentication).
func IdentityRefFrom(ctx context.Context) (IdentityRef, bool) {
	ref, ok := ctx.Value(ctxIdentityRef).(IdentityRef)
	return ref, ok
}
