// Package httpserver assembles the Orvexa API process: middleware chain,
// health endpoints and the /api/v1 route tree (mounted by later waves).
package httpserver

import (
        "net/http"

        "github.com/go-chi/chi/v5"
        "github.com/jackc/pgx/v5/pgxpool"

        "github.com/Roy-Wanyoike/orvexa/internal/platform/db"
        "github.com/Roy-Wanyoike/orvexa/internal/platform/httpx"
)

// Deps carries everything the API server needs.
type Deps struct {
        Pool   *pgxpool.Pool
        Domain DomainDeps
        Limiter *httpx.RateLimit
        Logger  httpx.Logger
}

// New builds the root handler.
func New(d Deps) http.Handler {
        if d.Logger == nil {
                d.Logger = func(string, ...any) {}
        }
        if d.Limiter == nil {
                d.Limiter = httpx.NewRateLimit(120, 60, 10_000)
        }
        r := chi.NewRouter()
        r.Use(httpx.RequestID)
        r.Use(httpx.SecurityHeaders)
        r.Use(httpx.Recoverer(d.Logger))

        r.Get("/healthz", handleLiveness)
        r.Get("/readyz", handleReadiness(d.Pool))

        MountV1(r, d.Domain, d.Limiter, d.Logger)
        return r
}

// handleLiveness always answers 200 — the process is alive.
func handleLiveness(w http.ResponseWriter, _ *http.Request) {
        httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "alive"}, nil)
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
                        "status": status,
                        "checks": []map[string]any{{"name": "db", "status": upDown(dbOK)}},
                }, nil)
        }
}

func upDown(ok bool) string {
        if ok {
                return "up"
        }
        return "down"
}
