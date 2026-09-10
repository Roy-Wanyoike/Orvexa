// Command api is the Orvexa HTTP API deployable: interaction-plane resources,
// control-plane auth, health endpoints. It boots with or without a reachable
// database (readiness reports the truth).
package main

import (
        "context"
        "errors"
        "fmt"
        "net/http"
        "os"
        "os/signal"
        "syscall"
        "time"

        "github.com/jackc/pgx/v5/pgxpool"

        "github.com/Roy-Wanyoike/orvexa/internal/httpserver"
        "github.com/Roy-Wanyoike/orvexa/internal/platform/db"
        "github.com/Roy-Wanyoike/orvexa/pkg/config"
        "github.com/Roy-Wanyoike/orvexa/pkg/logging"
)

func main() {
        cfg, err := config.Load()
        if err != nil {
                fmt.Fprintln(os.Stderr, "config load failed:", err)
                os.Exit(1)
        }
        log := logging.New(cfg.LogLevel, "orvexa-api", cfg.Env)

        ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
        defer stop()

        var pool *pgxpool.Pool
        if cfg.DatabaseURL != "" {
                p, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
                if err != nil {
                        log.Error("database connect failed (continuing degraded)", "err", err)
                } else {
                        pool = p
                        defer p.Close()
                }
        } else {
                log.Warn("no ORVEXA_DATABASE_URL configured — running without durable storage")
        }

        srv := &http.Server{
                Addr: cfg.HTTPAddr,
                Handler: httpserver.New(httpserver.Deps{
                        Pool: pool,
                }),
                ReadHeaderTimeout: 10 * time.Second,
        }

        errCh := make(chan error, 1)
        go func() { errCh <- srv.ListenAndServe() }()
        log.Info("orvexa-api listening", "addr", cfg.HTTPAddr)

        select {
        case <-ctx.Done():
                log.Info("shutdown signal received")
        case err := <-errCh:
                if err != nil && !errors.Is(err, http.ErrServerClosed) {
                        log.Error("server failed", "err", err)
                        os.Exit(1)
                }
        }

        shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()
        _ = srv.Shutdown(shutCtx)
        log.Info("orvexa-api stopped")
}
