// Command worker runs Orvexa's asynchronous subsystems: the transactional
// outbox dispatcher and event consumers (audit, analytics, usage metering).
// Consumer wiring lands with the waves that own them; this entry point always
// runs a supervision loop with graceful shutdown so consumers register into a
// stable process.
package main

import (
	"context"
	"fmt"
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
	log := logging.New(cfg.LogLevel, "orvexa-worker", cfg.Env)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("orvexa-worker starting", "bus", cfg.BusDriver)

	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("orvexa-worker stopped")
			return
		case <-tick.C:
			log.Debug("worker supervision heartbeat")
		}
	}
}
