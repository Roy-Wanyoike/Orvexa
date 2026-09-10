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
	Pool *pgxpool.Pool
}

// New builds the root handler.
func New(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.SecurityHeaders)

	r.Get("/healthz", handleLiveness)
	r.Get("/readyz", handleReadiness(d.Pool))
	r.Mount("/api", apiRouter())
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

// apiRouter mounts versioned API routes. Wave 1 ships the index; domain
// routers mount here in later waves.
func apiRouter() http.Handler {
	r := chi.NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"service": "orvexa-api",
			"version": "v1",
			"endpoints": []string{
				"/healthz", "/readyz",
				"/api/v1 (domain resources mount in subsequent waves)",
			},
		}, nil)
	})
	return r
}

func upDown(ok bool) string {
	if ok {
		return "up"
	}
	return "down"
}
