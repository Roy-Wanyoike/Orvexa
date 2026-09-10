// Command realtime is Orvexa's WebSocket gateway. It authenticates upgrades,
// subscribes to the event bus and fans events out per tenant/agent with
// connection caps. The hub implementation lands in wave O-6; this entry point
// owns process lifecycle, config and shutdown.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Roy-Wanyoike/orvexa/pkg/config"
	"github.com/Roy-Wanyoike/orvexa/pkg/logging"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(1)
	}
	log := logging.New(cfg.LogLevel, "orvexa-realtime", cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("/realtime", func(w http.ResponseWriter, _ *http.Request) {
		// Upgrade handling arrives with the O-6 hub; until then the endpoint
		// reports its planned state honestly instead of pretending.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":{"code":"realtime.not_wired","message":"websocket hub mounts in wave O-6"}}`))
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
