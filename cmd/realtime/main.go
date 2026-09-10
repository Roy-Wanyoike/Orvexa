// Command realtime is Orvexa's WebSocket gateway: authenticated upgrades,
// bus subscription and per-tenant/per-agent fan-out with connection caps.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Roy-Wanyoike/orvexa/internal/realtime"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/bus"
	"github.com/Roy-Wanyoike/orvexa/internal/platform/db"
	"github.com/Roy-Wanyoike/orvexa/internal/tenancy"
	"github.com/Roy-Wanyoike/orvexa/pkg/config"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
	"github.com/Roy-Wanyoike/orvexa/pkg/logging"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(1)
	}
	log := logging.New(cfg.LogLevel, "orvexa-realtime", cfg.Env)

	if cfg.DatabaseURL == "" {
		log.Error("ORVEXA_DATABASE_URL is required for the realtime gateway (socket auth)")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		log.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	auth := tenancy.NewService(pool)

	hub := realtime.NewHub(cfg.WSMaxPerPrincipal, cfg.WSMaxTotal, nil)
	theBus := bus.NewInProc()
	defer theBus.Close()

	// bus → hub fan-out
	unsub, err := theBus.Subscribe("*", func(_ context.Context, env *events.Envelope) error {
		hub.Publish(env)
		return nil
	})
	if err != nil {
		log.Error("bus subscribe failed", "err", err)
		os.Exit(1)
	}
	defer unsub()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "alive", "connections": hub.Count()})
	})
	mux.HandleFunc("/realtime", func(w http.ResponseWriter, r *http.Request) {
		hub.ServeHTTP(w, r, func(r *http.Request) (string, string, error) {
			key := r.Header.Get("X-API-Key")
			if key == "" {
				// browsers cannot set headers on WS upgrade: allow the
				// authenticated principal via first message instead of query
				// strings (no PII/secrets in URLs). Until client support
				// lands, header auth is the contract.
				return "", "", tenancy.ErrAuthRequired
			}
			p, err := auth.Authenticate(r.Context(), key)
			if err != nil {
				return "", "", err
			}
			agentID := r.URL.Query().Get("agent_id") // filter-only, not identity
			return p.TenantID, agentID, nil
		})
	})

	srv := &http.Server{
		Addr:              cfg.RealtimeAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("orvexa-realtime listening", "addr", cfg.RealtimeAddr)

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Error("server failed", "err", err)
			os.Exit(1)
		}
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Info("orvexa-realtime stopped")
}
