// Package httpserver assembles the Orvexa API process: middleware chain,
// health endpoints and the /api/v1 route tree (mounted by later waves).
package httpserver

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Roy-Wanyoike/orvexa/internal/identity"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/buildinfo"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/db"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
)

// Deps carries everything the API server needs.
type Deps struct {
	Pool    *pgxpool.Pool
	Domain  DomainDeps
	Limiter *httpx.RateLimit
	Logger  httpx.Logger

	// Identity is the optional OIDC identity plane ([O-29], issue #38).
	// When nil, New consults identity.FromEnv(): the ORVEXA_OIDC_* environment
	// is the production wiring point. Nil AND env unset ⇒ OIDC disabled and
	// the API-key path is byte-identical to the pre-[O-29] stack.
	Identity *identity.Service
}

// New builds the root handler.
func New(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = func(string, ...any) {}
	}
	if d.Limiter == nil {
		d.Limiter = httpx.NewRateLimit(120, 60, 10_000)
	}

	// [O-29] additive: OIDC identity plane — enabled ONLY when configured.
	// Env unset + Deps.Identity nil leaves the stack API-key-only. With a
	// database pool, role_bindings (migration 0012_rbac.sql) are authoritative
	// behind the short-TTL resolver; without one the static (roles claim)
	// posture applies and says so via the log line below.
	if d.Identity == nil {
		if svc, ok := identity.FromEnv(); ok {
			d.Identity = svc
		}
	}
	if d.Identity != nil {
		if d.Pool != nil && !d.Identity.HasRoleResolver() {
			d.Identity = d.Identity.WithRoleResolver(identity.NewDBRoleResolver(d.Pool, 0, 0))
		}
		d.Identity = d.Identity.WithLogger(d.Logger)
		if !d.Identity.HasRoleResolver() {
			d.Logger("oidc role bindings: static posture (roles claim trusted; no database pool wired)")
		}
	}

	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.SecurityHeaders)
	r.Use(httpx.Recoverer(d.Logger))

	r.Get("/healthz", handleLiveness)
	r.Get("/readyz", handleReadiness(d.Pool))

	// authenticated API traffic: per-key limits
	// webhook ingress: per-IP limits (providers don't hold API keys)
	MountV1(r, d.Domain, d.Limiter, httpx.NewRateLimit(120, 60, 10_000), d.Logger, d.Identity)
	return r
}

// handleLiveness always answers 200 — the process is alive. The version
// field ([O-37] issue #46) is additive: ldflags-stamped release identity so
// operations can identify any running build.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "alive",
		"version": buildinfo.String(),
	}, nil)
}

// handleReadiness reports component health: the database.
func handleReadiness(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbOK := db.Healthy(r.Context(), pool)
		status := "ready"
		code := http.StatusOK
		if !dbOK {
			status = "degraded"
			code = http.StatusServiceUnavailable
		}
		httpx.WriteJSON(w, code, map[string]any{
			"status":  status,
			"version": buildinfo.String(), // additive stamp ([O-37] issue #46)
			"checks":  []map[string]any{{"name": "db", "status": upDown(dbOK)}},
		}, nil)
	}
}

func upDown(ok bool) string {
	if ok {
		return "up"
	}
	return "down"
}
