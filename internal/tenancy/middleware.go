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
